package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// A send with no feed sighting after this long is reported as not landed.
// Intents live at most two seconds and route deadlines at most thirty.
const landingGrace = time.Minute

// BidView is the competition model's tip decision for an intent.
type BidView struct {
	ModelTip string `json:"model_tip"`
	Wins     int    `json:"wins"`
	Samples  int    `json:"samples"`
	Applied  bool   `json:"applied"`
	// Win is the win probability of the model tip; zero in decisions made
	// before the model estimated one.
	Win   float64 `json:"win_probability,omitempty"`
	Probe bool    `json:"probe,omitempty"`
}

type Landing struct {
	Block        uint64 `json:"block"`
	Frame        uint64 `json:"frame"`
	Index        uint64 `json:"index"`
	ReceivedNS   int64  `json:"received_ns"`
	SendToFeedNS *int64 `json:"send_to_feed_ns,omitempty"`
	Success      bool   `json:"success"`
	Invalidated  bool   `json:"invalidated"`
}

// Settled is a booked outcome. Net, fees and gas are empty while FeeStatus
// is "pending" (receipt not read yet) or "unknown" (it never came).
type Settled struct {
	Block     uint64   `json:"block"`
	AtMS      int64    `json:"at_ms"`
	Success   bool     `json:"success"`
	Cancelled bool     `json:"cancelled"`
	Profit    string   `json:"profit"`
	Net       string   `json:"net"`
	Fee       string   `json:"fee"`
	L1Fee     string   `json:"l1_fee"`
	GasUsed   uint64   `json:"gas_used"`
	NetNative *float64 `json:"net_native,omitempty"`
	FeeStatus string   `json:"fee_status,omitempty"`
}

// Rival is the earliest successful competitor that touched one of our route's
// pools after our trigger and before our landing.
type Rival struct {
	Sender string `json:"sender"`
	Hash   string `json:"hash"`
	Block  uint64 `json:"block"`
	Frame  uint64 `json:"frame"`
	Index  uint64 `json:"index"`
	Tip    string `json:"tip"`
}

// Send is one signed transaction and everything known about its outcome.
type Send struct {
	Hash          string       `json:"hash"`
	IntentID      string       `json:"intent_id"`
	Action        string       `json:"action"`
	SentMS        int64        `json:"sent_ms"`
	Asset         string       `json:"asset"`
	Input         string       `json:"input"`
	ExpectedNet   string       `json:"expected_net"`
	QuotedGross   string       `json:"quoted_gross"`
	Budget        string       `json:"budget"`
	Tip           string       `json:"tip"`
	GasLimit      uint64       `json:"gas_limit"`
	ConvNum       string       `json:"conv_num"`
	ConvDen       string       `json:"conv_den"`
	Bid           *BidView     `json:"bid,omitempty"`
	Route         []poolRef    `json:"route"`
	TriggerBlock  uint64       `json:"trigger_block"`
	TriggerSource string       `json:"trigger_source"`
	Lane          string       `json:"lane"`
	ReceivedNS    int64        `json:"received_ns"`
	ExecuteNS     int64        `json:"execute_ns"`
	AckNS         *int64       `json:"ack_ns,omitempty"`
	Error         string       `json:"error"`
	Paths         []PathResult `json:"paths"`
	Landing       *Landing     `json:"landing,omitempty"`
	Settled       *Settled     `json:"settled,omitempty"`
	Outcome       string       `json:"outcome"`
	Rival         *Rival       `json:"rival,omitempty"`
}

// Outcome filters match the outcome names Send reports.
var outcomeFilters = map[string]string{
	"":         "1",
	"success":  "success=1 AND cancelled=0",
	"reverted": "success=0 AND cancelled=0",
	"unlanded": "success IS NULL AND (landed_block IS NULL OR invalidated=1)",
}

const sendColumns = `hash, intent_id, action, sent_ms, asset, input, expected_net, quoted_gross, budget, tip, gas_limit, conv_num, conv_den, bid, route,
	trigger_block, trigger_source, lane, received_ns, execute_ns, ack_ns, error, paths,
	landed_block, landed_frame, landed_index, landed_ns, send_to_feed_ns, landed_success, invalidated,
	settled_block, settled_ms, success, cancelled, profit, net, fee, l1_fee, gas_used, net_native, fee_status`

// Sends lists the newest transactions, filtered by outcome.
func (s *Store) Sends(ctx context.Context, instance, outcome string, limit int, now time.Time) ([]Send, error) {
	filter, ok := outcomeFilters[outcome]
	if !ok {
		return nil, fmt.Errorf("%w: outcome %q", errBadQuery, outcome)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT `+sendColumns+` FROM sends WHERE instance=? AND `+filter+` ORDER BY sent_ms DESC, hash LIMIT ?`, instance, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sends := []Send{}
	for rows.Next() {
		send, err := scanSend(rows, now)
		if err != nil {
			return nil, err
		}
		sends = append(sends, send)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range sends {
		if sends[i].Outcome != "reverted" && sends[i].Outcome != "unlanded" {
			continue
		}
		if sends[i].Rival, err = s.rival(ctx, instance, sends[i]); err != nil {
			return nil, err
		}
	}
	return sends, nil
}

func scanSend(rows *sql.Rows, now time.Time) (Send, error) {
	var send Send
	var bid, route, paths string
	var landedBlock, landedFrame, landedIndex, landedNS, sendToFeed, settledBlock, settledMS, gasUsed sql.NullInt64
	var landedSuccess, success sql.NullBool
	var invalidated, cancelled bool
	var profit, net, fee, l1Fee, feeStatus string
	var netNative sql.NullFloat64
	var ack sql.NullInt64
	if err := rows.Scan(&send.Hash, &send.IntentID, &send.Action, &send.SentMS, &send.Asset, &send.Input, &send.ExpectedNet, &send.QuotedGross, &send.Budget, &send.Tip,
		&send.GasLimit, &send.ConvNum, &send.ConvDen, &bid, &route, &send.TriggerBlock, &send.TriggerSource, &send.Lane, &send.ReceivedNS, &send.ExecuteNS, &ack, &send.Error, &paths,
		&landedBlock, &landedFrame, &landedIndex, &landedNS, &sendToFeed, &landedSuccess, &invalidated,
		&settledBlock, &settledMS, &success, &cancelled, &profit, &net, &fee, &l1Fee, &gasUsed, &netNative, &feeStatus); err != nil {
		return send, err
	}
	if ack.Valid {
		send.AckNS = &ack.Int64
	}
	if bid != "" {
		send.Bid = &BidView{}
		if err := json.Unmarshal([]byte(bid), send.Bid); err != nil {
			return send, fmt.Errorf("decode stored bid: %w", err)
		}
	}
	if err := json.Unmarshal([]byte(route), &send.Route); err != nil {
		return send, fmt.Errorf("decode stored route: %w", err)
	}
	send.Paths = []PathResult{}
	if paths != "" {
		if err := json.Unmarshal([]byte(paths), &send.Paths); err != nil {
			return send, fmt.Errorf("decode stored paths: %w", err)
		}
	}
	if landedBlock.Valid {
		send.Landing = &Landing{Block: uint64(landedBlock.Int64), Frame: uint64(landedFrame.Int64), Index: uint64(landedIndex.Int64), ReceivedNS: landedNS.Int64,
			Success: landedSuccess.Bool, Invalidated: invalidated}
		if sendToFeed.Valid {
			send.Landing.SendToFeedNS = &sendToFeed.Int64
		}
	}
	if success.Valid {
		send.Settled = &Settled{Block: uint64(settledBlock.Int64), AtMS: settledMS.Int64, Success: success.Bool, Cancelled: cancelled, Profit: profit, Net: net, Fee: fee, L1Fee: l1Fee,
			GasUsed: uint64(gasUsed.Int64), FeeStatus: feeStatus}
		if netNative.Valid {
			send.Settled.NetNative = &netNative.Float64
		}
	}
	send.Outcome = outcome(send, now)
	return send, nil
}

func outcome(send Send, now time.Time) string {
	switch {
	case send.Settled != nil && send.Settled.Cancelled:
		return "cancelled"
	case send.Settled != nil && send.Settled.Success:
		return "success"
	case send.Settled != nil:
		return "reverted"
	case send.Landing != nil && !send.Landing.Invalidated:
		return "landed"
	case now.Sub(time.UnixMilli(send.SentMS)) < landingGrace:
		return "pending"
	default:
		return "unlanded"
	}
}

func (s *Store) rival(ctx context.Context, instance string, send Send) (*Rival, error) {
	if send.TriggerBlock == 0 || len(send.Route) == 0 {
		return nil, nil
	}
	last := send.TriggerBlock + 2
	if send.Landing != nil {
		last = send.Landing.Block
	}
	rows, err := s.db.QueryContext(ctx, `SELECT sender, hash, block, frame, tx_index, tip, pools FROM rivals
		WHERE instance=? AND block BETWEEN ? AND ? AND success=1 AND received_ms > ? ORDER BY block, frame, tx_index LIMIT 200`,
		instance, send.TriggerBlock, last, send.ReceivedNS/1e6)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ours := make(map[string]bool, len(send.Route))
	for _, pool := range send.Route {
		ours[pool.Address] = true
	}
	for rows.Next() {
		var rival Rival
		var encoded string
		if err := rows.Scan(&rival.Sender, &rival.Hash, &rival.Block, &rival.Frame, &rival.Index, &rival.Tip, &encoded); err != nil {
			return nil, err
		}
		if landing := send.Landing; landing != nil && !before(rival, *landing) {
			break
		}
		var pools []poolRef
		if err := json.Unmarshal([]byte(encoded), &pools); err != nil {
			return nil, fmt.Errorf("decode stored rival pools: %w", err)
		}
		if slices.ContainsFunc(pools, func(pool poolRef) bool { return ours[pool.Address] }) {
			return &rival, nil
		}
	}
	return nil, rows.Err()
}

func before(rival Rival, landing Landing) bool {
	if rival.Block != landing.Block {
		return rival.Block < landing.Block
	}
	if rival.Frame != landing.Frame {
		return rival.Frame < landing.Frame
	}
	return rival.Index < landing.Index
}
