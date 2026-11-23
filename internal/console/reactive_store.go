package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// column is one value of a send. keep leaves an earlier non-empty value: the
// send timestamp from the intent record outranks the seal time a settlement
// carries.
type column struct {
	name  string
	value any
	keep  bool
}

// Lines about one transaction arrive separately (intent, path timings, feed
// observation, settlement); each fills its own columns of the same row.
func upsertSend(ctx context.Context, tx *sql.Tx, instance, hash string, columns []column) error {
	names := []string{"instance", "hash"}
	marks := []string{"?", "?"}
	updates := make([]string, 0, len(columns))
	arguments := []any{instance, hash}
	for _, c := range columns {
		names = append(names, c.name)
		marks = append(marks, "?")
		arguments = append(arguments, c.value)
		if c.keep {
			updates = append(updates, fmt.Sprintf("%[1]s=CASE WHEN sends.%[1]s IN (0, '') THEN excluded.%[1]s ELSE sends.%[1]s END", c.name))
		} else {
			updates = append(updates, fmt.Sprintf("%[1]s=excluded.%[1]s", c.name))
		}
	}
	statement := fmt.Sprintf("INSERT INTO sends(%s) VALUES(%s) ON CONFLICT(instance, hash) DO UPDATE SET %s",
		strings.Join(names, ", "), strings.Join(marks, ", "), strings.Join(updates, ", "))
	_, err := tx.ExecContext(ctx, statement, arguments...)
	return err
}

func intentColumns(send sendRow) ([]column, error) {
	route, err := json.Marshal(send.Route)
	if err != nil {
		return nil, err
	}
	return []column{
		{"intent_id", send.IntentID, false}, {"asset", send.Asset, false}, {"input", send.Input, false}, {"expected_net", send.ExpectedNet, false},
		{"quoted_gross", send.QuotedGross, false}, {"budget", send.Budget, false}, {"tip", send.Tip, false}, {"gas_limit", send.GasLimit, false},
		{"conv_num", send.ConvNum, false}, {"conv_den", send.ConvDen, false}, {"bid", send.Bid, false}, {"route", string(route), false},
		{"trigger_block", send.TriggerBlock, false}, {"trigger_source", send.TriggerSource, false}, {"sent_ms", send.SentMS, true},
	}, nil
}

func writeBatch(ctx context.Context, tx *sql.Tx, instance string, records *batch) error {
	for _, send := range records.sends {
		columns, err := intentColumns(send)
		if err != nil {
			return err
		}
		columns[len(columns)-1].keep = false
		columns = append(columns, column{"action", send.Action, false}, column{"lane", send.Lane, false}, column{"received_ns", send.ReceivedNS, false},
			column{"execute_ns", send.ExecuteNS, false}, column{"ack_ns", send.AckNS, false}, column{"error", send.Error, false})
		if err := upsertSend(ctx, tx, instance, send.Hash, columns); err != nil {
			return err
		}
	}
	for _, paths := range records.paths {
		encoded, err := json.Marshal(paths.Paths)
		if err != nil {
			return err
		}
		if err := upsertSend(ctx, tx, instance, paths.Hash, []column{{"paths", string(encoded), false}}); err != nil {
			return err
		}
	}
	for _, landing := range records.landings {
		if err := upsertSend(ctx, tx, instance, landing.Hash, []column{
			{"landed_block", landing.Block, false}, {"landed_frame", landing.Frame, false}, {"landed_index", landing.Index, false},
			{"landed_ns", landing.ReceivedNS, false}, {"send_to_feed_ns", landing.SendToFeedNS, false}, {"landed_success", landing.Success, false}, {"invalidated", false, false},
		}); err != nil {
			return err
		}
	}
	for _, hash := range records.voided {
		if err := upsertSend(ctx, tx, instance, hash, []column{{"invalidated", true, false}}); err != nil {
			return err
		}
	}
	for _, settled := range records.settled {
		columns := []column{
			{"settled_block", settled.Block, false}, {"settled_ms", settled.AtMS, false}, {"success", settled.Success, false}, {"cancelled", settled.Cancelled, false},
			{"profit", settled.Profit, false}, {"net", settled.Net, false}, {"fee", settled.Fee, false}, {"l1_fee", settled.L1Fee, false}, {"gas_used", settled.GasUsed, false},
			{"profit_value", settled.ProfitValue, false}, {"net_value", settled.NetValue, false}, {"fee_value", settled.FeeValue, false},
			{"profit_native", settled.ProfitNative, false}, {"net_native", settled.NetNative, false}, {"fee_status", settled.FeeStatus, false},
		}
		if settled.intent != nil {
			intent, err := intentColumns(*settled.intent)
			if err != nil {
				return err
			}
			columns = append(columns, intent...)
		}
		if err := upsertSend(ctx, tx, instance, settled.Hash, columns); err != nil {
			return err
		}
	}
	// A canonical block chose another variant of the nonce. Cleared after the
	// settlements, as the void always follows the booking it replaces.
	for _, hash := range records.unsettled {
		if err := upsertSend(ctx, tx, instance, hash, []column{
			{"settled_block", nil, false}, {"settled_ms", nil, false}, {"success", nil, false}, {"cancelled", false, false},
			{"profit", "", false}, {"net", "", false}, {"fee", "", false}, {"l1_fee", "", false}, {"gas_used", nil, false},
			{"profit_value", nil, false}, {"net_value", nil, false}, {"fee_value", nil, false}, {"profit_native", nil, false}, {"net_native", nil, false}, {"fee_status", "", false},
		}); err != nil {
			return err
		}
	}
	for _, skip := range records.skips {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reactive_skips(instance, to_ms, reason, count) VALUES(?,?,?,?)`, instance, skip.ToMS, skip.Reason, skip.Count); err != nil {
			return err
		}
	}
	for _, latest := range records.latest {
		if _, err := tx.ExecContext(ctx, `INSERT INTO reactive_latest(instance, type, at_ms, body) VALUES(?,?,?,?)
			ON CONFLICT(instance, type) DO UPDATE SET at_ms=excluded.at_ms, body=excluded.body WHERE excluded.at_ms >= reactive_latest.at_ms`,
			instance, latest.Type, latest.AtMS, string(latest.Body)); err != nil {
			return err
		}
	}
	for _, token := range records.assets {
		if _, err := tx.ExecContext(ctx, `INSERT INTO assets(instance, token, symbol, decimals) VALUES(?,?,?,?)
			ON CONFLICT(instance, token) DO UPDATE SET symbol=excluded.symbol, decimals=excluded.decimals`, instance, token.Address.Hex(), token.Symbol, token.Decimals); err != nil {
			return err
		}
	}
	if err := writeCandidates(ctx, tx, instance, records.candidates); err != nil {
		return err
	}
	for _, rival := range records.rivals {
		pools, err := json.Marshal(rival.Pools)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO rivals(instance, block, frame, tx_index, hash, sender, tip, tip_value, success, pools, received_ms) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			instance, rival.Block, rival.Frame, rival.Index, rival.Hash, rival.Sender, rival.Tip, rival.TipValue, rival.Success, string(pools), rival.ReceivedMS); err != nil {
			return err
		}
	}
	return nil
}
