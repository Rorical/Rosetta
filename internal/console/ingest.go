package console

import (
	"encoding/json"
	"fmt"
	"math/big"
	"strconv"

	"github.com/Rorical/Rosetta/internal/domain"
)

const (
	kindReactive    = "reactive"
	kindCompetition = "competition"
)

// The decoders name only the fields the console shows. They use domain types,
// so a renamed field fails the contract tests, which encode the bot's own
// structs. Hashes and addresses travel as byte arrays and integers as JSON
// numbers, exactly as the bot writes them.

type intentLine struct {
	ID      domain.Hash
	Version domain.StateVersion
	Route   struct {
		Asset  domain.Address
		Amount *big.Int
		Steps  []struct{ Pool domain.PoolKey }
	}
	Call struct {
		GasLimit uint64
		TipCap   *big.Int
	}
	QuotedGross, Budget, ExpectedNet *big.Int
	Conversion                       struct{ Numerator, Denominator *big.Int }
	Bid                              *struct {
		ModelTip      *big.Int
		Wins, Samples int
		Applied       bool
		Win           float64
		Probe         bool
	}
	CreatedMS int64
}

type submissionLine struct {
	StartedAtNS, ReturnedAtNS int64
	Acknowledged              bool
}

type reactiveLine struct {
	Type   string `json:"type"`
	Action string `json:"action"`
	Status string `json:"status"`
	Intent *intentLine
	Timing struct {
		ReceivedNS int64  `json:"received_ns"`
		ExecuteNS  int64  `json:"execute_ns"`
		Lane       string `json:"lane"`
	} `json:"timing"`
	Transaction *domain.Hash    `json:"transaction"`
	Submission  *submissionLine `json:"submission"`
	Error       string          `json:"error"`
	// execution_observed uses the bot's exported field names.
	Hash             domain.Hash
	Position         *positionLine
	SendToObservedNS *int64
	Success          bool
	Cancelled        bool
	// settlement
	Receipt *settlementLine `json:"receipt"`
	// submission_paths
	Paths []PathResult `json:"paths"`
	// reactive_skips
	ToMS    int64             `json:"to_ms"`
	Reasons map[string]uint64 `json:"reasons"`
	// route_survival
	AtMS     int64             `json:"at_ms"`
	Survival map[string][2]int `json:"survived_of_total_by_hops"`
	// reactive_assets
	Assets []domain.Token `json:"assets"`
}

type positionLine struct {
	BlockNumber, FrameIndex, TransactionIndex uint64
	ReceivedAtNS                              int64
}

type settlementLine struct {
	Cancelled            bool
	Intent               *intentLine
	Hash                 domain.Hash
	Block                domain.BlockRef
	Position             *positionLine
	Success              bool
	Asset                domain.Address
	Profit, LoanFee, Net *big.Int
	Fees                 *struct {
		GasUsed uint64   `json:"gas_used"`
		L1      *big.Int `json:"l1_fee"`
		Total   *big.Int `json:"total_fee"`
	}
	// FeeStatus is "pending" while the bot waits for the receipt and "unknown"
	// when it never came; Fees and Net are absent in both.
	FeeStatus string
}

type dispatchLine struct {
	AtMS           int64        `json:"at_ms"`
	HaltedReason   string       `json:"halted_reason"`
	Stopped        bool         `json:"stopped"`
	NextNonce      uint64       `json:"next_nonce"`
	Pending        *domain.Hash `json:"pending"`
	Variants       int          `json:"variants"`
	Included       int          `json:"included"`
	SpentToday     *big.Int     `json:"spent_today"`
	DailyCap       *big.Int     `json:"daily_cap"`
	PerTransaction *big.Int     `json:"transaction_cap"`
}

// PathResult is one submission path's answer to a send, as the bot records it.
type PathResult struct {
	Name      string `json:"name"`
	ElapsedMS int64  `json:"elapsed_ms"`
	Accepted  bool   `json:"accepted"`
	Error     string `json:"error,omitempty"`
}

// batch holds what one read of a file adds to the database, in file order.
type batch struct {
	sends      []sendRow
	landings   []landingRow
	voided     []string
	settled    []settlementRow
	unsettled  []string
	paths      []pathsRow
	skips      []skipRow
	latest     []latestRow
	assets     []domain.Token
	rivals     []rivalRow
	candidates []candidateRow
}

func (b *batch) empty() bool {
	return len(b.sends)+len(b.landings)+len(b.voided)+len(b.settled)+len(b.unsettled)+len(b.paths)+len(b.skips)+len(b.latest)+len(b.assets)+len(b.rivals)+len(b.candidates) == 0
}

// sendRow is the intent-side description of one signed transaction.
type sendRow struct {
	Hash, IntentID, Action, Asset, Input, ExpectedNet, QuotedGross, Budget, Tip, ConvNum, ConvDen, Bid, Error, Lane string
	Route                                                                                                           []poolRef
	GasLimit, TriggerBlock                                                                                          uint64
	TriggerSource                                                                                                   string
	SentMS, ReceivedNS, ExecuteNS                                                                                   int64
	AckNS                                                                                                           *int64
}

type landingRow struct {
	Hash                string
	Block, Frame, Index uint64
	ReceivedNS          int64
	SendToFeedNS, AckNS *int64
	Success             bool
}

type settlementRow struct {
	intent                        *sendRow
	Hash, Profit, Net, Fee, L1Fee string
	FeeStatus                     string
	Block                         uint64
	AtMS                          int64
	GasUsed                       *uint64
	Success, Cancelled            bool
	ProfitValue                   float64
	NetValue, FeeValue            *float64
	ProfitNative, NetNative       *float64
}

type pathsRow struct {
	Hash  string
	Paths []PathResult
}

type skipRow struct {
	ToMS   int64
	Reason string
	Count  uint64
}

// latestRow keeps the newest record of a kind that replaces the previous one.
type latestRow struct {
	Type string
	AtMS int64
	Body []byte
}

// Record types the console does not show are ignored, not counted as errors.
func parseReactive(line []byte, into *batch) error {
	var record reactiveLine
	if err := json.Unmarshal(line, &record); err != nil {
		return fmt.Errorf("decode reactive record: %w", err)
	}
	switch record.Type {
	case "reactive_intent":
		send, ok := intentSend(record)
		if ok {
			into.sends = append(into.sends, send)
		}
	case "execution_observed":
		if record.Position == nil {
			return fmt.Errorf("execution observation lacks a position")
		}
		landing := landingRow{Hash: record.Hash.Hex(), Block: record.Position.BlockNumber, Frame: record.Position.FrameIndex, Index: record.Position.TransactionIndex,
			ReceivedNS: record.Position.ReceivedAtNS, SendToFeedNS: record.SendToObservedNS, Success: record.Success}
		into.landings = append(into.landings, landing)
	case "execution_observation_invalidated":
		if record.Transaction == nil {
			return fmt.Errorf("invalidation lacks a transaction")
		}
		into.voided = append(into.voided, record.Transaction.Hex())
	case "settlement":
		settled, err := settlement(record.Receipt)
		if err != nil {
			return err
		}
		into.settled = append(into.settled, settled)
	case "settlement_voided":
		if record.Transaction == nil {
			return fmt.Errorf("voided settlement lacks a transaction")
		}
		into.unsettled = append(into.unsettled, record.Transaction.Hex())
	case "submission_paths":
		if record.Transaction != nil && *record.Transaction != (domain.Hash{}) {
			into.paths = append(into.paths, pathsRow{Hash: record.Transaction.Hex(), Paths: record.Paths})
		}
	case "reactive_skips":
		for reason, count := range record.Reasons {
			into.skips = append(into.skips, skipRow{ToMS: record.ToMS, Reason: reason, Count: count})
		}
	case "route_survival":
		body, err := json.Marshal(survivalRows(record.Survival))
		if err != nil {
			return err
		}
		into.latest = append(into.latest, latestRow{Type: record.Type, AtMS: record.AtMS, Body: body})
	case "dispatch_state":
		body, err := dispatchBody(line)
		if err != nil {
			return err
		}
		into.latest = append(into.latest, latestRow{Type: record.Type, AtMS: record.AtMS, Body: body})
	case "reactive_assets":
		into.assets = append(into.assets, record.Assets...)
	}
	return nil
}

// intentSend keeps only intents that produced a transaction. Shadow, held and
// refused intents never reached the network.
func intentSend(record reactiveLine) (sendRow, bool) {
	if record.Intent == nil || record.Transaction == nil || *record.Transaction == (domain.Hash{}) {
		return sendRow{}, false
	}
	send := describeIntent(record.Intent)
	send.Hash, send.Action, send.Error, send.Lane = record.Transaction.Hex(), record.Action, record.Error, record.Timing.Lane
	send.ReceivedNS, send.ExecuteNS = record.Timing.ReceivedNS, record.Timing.ExecuteNS
	send.SentMS = record.Timing.ExecuteNS / 1e6
	if record.Submission != nil {
		send.SentMS = record.Submission.StartedAtNS / 1e6
		send.ExecuteNS = record.Submission.StartedAtNS
		if record.Submission.ReturnedAtNS >= record.Submission.StartedAtNS {
			ack := record.Submission.ReturnedAtNS - record.Submission.StartedAtNS
			send.AckNS = &ack
		}
	}
	return send, true
}

func describeIntent(intent *intentLine) sendRow {
	pools := make([]domain.PoolKey, 0, len(intent.Route.Steps))
	for _, step := range intent.Route.Steps {
		pools = append(pools, step.Pool)
	}
	send := sendRow{
		IntentID: intent.ID.Hex(), Asset: intent.Route.Asset.Hex(), Input: decimal(intent.Route.Amount), ExpectedNet: decimal(intent.ExpectedNet),
		QuotedGross: decimal(intent.QuotedGross), Budget: decimal(intent.Budget), Tip: decimal(intent.Call.TipCap), GasLimit: intent.Call.GasLimit,
		ConvNum: decimal(intent.Conversion.Numerator), ConvDen: decimal(intent.Conversion.Denominator), Route: poolRefs(pools),
		TriggerBlock: intent.Version.BlockNumber, TriggerSource: sourceName(intent.Version.Source), SentMS: intent.CreatedMS,
	}
	if intent.Bid != nil {
		bid, _ := json.Marshal(BidView{ModelTip: decimal(intent.Bid.ModelTip), Wins: intent.Bid.Wins, Samples: intent.Bid.Samples, Applied: intent.Bid.Applied, Win: intent.Bid.Win, Probe: intent.Bid.Probe})
		send.Bid = string(bid)
	}
	return send
}

// settlement reads a booked outcome. The bot books a feed sighting before its
// receipt, so fees may still be pending; a later record for the same hash
// replaces the row.
func settlement(receipt *settlementLine) (settlementRow, error) {
	if receipt == nil {
		return settlementRow{}, fmt.Errorf("settlement lacks a receipt")
	}
	paid := receipt.Fees != nil && receipt.Fees.Total != nil
	if !paid && receipt.FeeStatus == "" {
		return settlementRow{}, fmt.Errorf("settlement lacks receipt fees")
	}
	profit := orZero(receipt.Profit)
	row := settlementRow{Hash: receipt.Hash.Hex(), Profit: profit.String(), FeeStatus: receipt.FeeStatus, Block: receipt.Block.Number, AtMS: int64(receipt.Block.Timestamp) * 1000,
		Success: receipt.Success, Cancelled: receipt.Cancelled, ProfitValue: approximate(profit)}
	if row.AtMS == 0 && receipt.Position != nil {
		row.AtMS = receipt.Position.ReceivedAtNS / 1e6
	}
	if paid {
		net, fee := orZero(receipt.Net), approximate(receipt.Fees.Total)
		netValue := approximate(net)
		row.Net, row.Fee, row.L1Fee = net.String(), receipt.Fees.Total.String(), decimal(receipt.Fees.L1)
		row.GasUsed, row.NetValue, row.FeeValue = &receipt.Fees.GasUsed, &netValue, &fee
	}
	if receipt.Intent != nil {
		intent := describeIntent(receipt.Intent)
		row.intent = &intent
		// Native values use the intent's frozen admission conversion, the same
		// basis the bot used for its own net.
		if native := toNative(profit, receipt.Intent.Conversion.Numerator, receipt.Intent.Conversion.Denominator); native != nil {
			profitNative := approximate(native)
			row.ProfitNative = &profitNative
			if paid {
				netNative := approximate(new(big.Int).Sub(native, receipt.Fees.Total))
				row.NetNative = &netNative
			}
		}
	}
	return row, nil
}

// toNative converts an asset amount at numerator asset units per denominator wei.
func toNative(amount, numerator, denominator *big.Int) *big.Int {
	if numerator == nil || denominator == nil || numerator.Sign() <= 0 || denominator.Sign() <= 0 {
		return nil
	}
	value := new(big.Int).Mul(amount, denominator)
	return value.Quo(value, numerator)
}

// SurvivalRow is one route length's paper survival: of routes that executed at
// their quote state, how many still executed at the last delay.
type SurvivalRow struct {
	Hops     int `json:"hops"`
	Survived int `json:"survived"`
	Total    int `json:"total"`
}

func survivalRows(byHops map[string][2]int) []SurvivalRow {
	rows := make([]SurvivalRow, 0, len(byHops))
	for hops, counts := range byHops {
		value, err := strconv.Atoi(hops)
		if err != nil {
			continue
		}
		rows = append(rows, SurvivalRow{Hops: value, Survived: counts[0], Total: counts[1]})
	}
	return rows
}

// DispatchView is the dispatcher's last reported lane and budget, in wei.
type DispatchView struct {
	AtMS           int64  `json:"at_ms"`
	HaltedReason   string `json:"halted_reason"`
	Stopped        bool   `json:"stopped"`
	NextNonce      uint64 `json:"next_nonce"`
	Pending        string `json:"pending"`
	Variants       int    `json:"variants"`
	Included       int    `json:"included"`
	SpentToday     string `json:"spent_today"`
	DailyCap       string `json:"daily_cap"`
	PerTransaction string `json:"transaction_cap"`
}

func dispatchBody(line []byte) ([]byte, error) {
	var state dispatchLine
	if err := json.Unmarshal(line, &state); err != nil {
		return nil, fmt.Errorf("decode dispatch state: %w", err)
	}
	view := DispatchView{AtMS: state.AtMS, HaltedReason: state.HaltedReason, Stopped: state.Stopped, NextNonce: state.NextNonce, Variants: state.Variants, Included: state.Included,
		SpentToday: decimal(state.SpentToday), DailyCap: decimal(state.DailyCap), PerTransaction: decimal(state.PerTransaction)}
	if state.Pending != nil {
		view.Pending = state.Pending.Hex()
	}
	return json.Marshal(view)
}

func decimal(value *big.Int) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func orZero(value *big.Int) *big.Int {
	if value == nil {
		return new(big.Int)
	}
	return value
}

// approximate gives sums and sorting a float; the exact decimal stays in the row.
func approximate(value *big.Int) float64 {
	result, _ := new(big.Float).SetInt(value).Float64()
	return result
}

func sourceName(source domain.StateSource) string {
	if source == domain.SourceFast {
		return "fast"
	}
	return "standard"
}
