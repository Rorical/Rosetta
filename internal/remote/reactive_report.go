package remote

import (
	"bytes"
	"context"
	"encoding/binary"
	"log/slog"
	"math"
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// The records and metrics below exist for the operator console. None of them
// gates a send: a lost report never changes what the bot does.

// readAssets reads each start asset's symbol and decimals once, so the console
// can show amounts in token units. A token that does not answer decimals() is
// left out and the console shows its base units. The zero address is the
// native token, which is ETH on every supported chain.
func (s *reactiveRuntime) readAssets(ctx context.Context, block rpc.ExecutionBlock) []domain.Token {
	reader := &snapshot{client: s.client, block: block, chain: s.settings.planning.Chain}
	defer reader.Close()
	tokens := make([]domain.Token, 0, len(s.settings.minimum))
	for address := range s.settings.minimum {
		if address == (domain.Address{}) {
			tokens = append(tokens, domain.Token{Address: address, Symbol: "ETH", Decimals: 18})
			continue
		}
		decimals, err := reactiveWord(ctx, reader, address, "decimals()")
		if err != nil || !decimals.IsUint64() || decimals.Uint64() > 36 {
			slog.Warn("start asset decimals unavailable", "token", address.Hex())
			continue
		}
		token := domain.Token{Address: address, Decimals: uint8(decimals.Uint64())}
		if raw, err := reader.ReadContract(ctx, address, reactiveData("symbol()")); err == nil {
			token.Symbol = decodeSymbol(raw)
		}
		tokens = append(tokens, token)
	}
	slices.SortFunc(tokens, func(a, b domain.Token) int { return bytes.Compare(a.Address[:], b.Address[:]) })
	return tokens
}

// maxSymbol bounds what a token contract can put into the console.
const maxSymbol = 16

// decodeSymbol accepts the ABI string most tokens return and the bytes32 some
// older ones do. Anything else, or text outside printable ASCII, gives "".
func decodeSymbol(raw []byte) string {
	text := raw
	if len(raw) == 32 {
		text = raw[:len(trimZeros(raw))]
	} else if len(raw) >= 64 {
		offset := wordInt(raw[:32])
		if offset < 0 || offset > len(raw)-32 {
			return ""
		}
		length := wordInt(raw[offset : offset+32])
		start := offset + 32
		if length < 0 || length > len(raw)-start {
			return ""
		}
		text = raw[start : start+length]
	} else {
		return ""
	}
	if len(text) == 0 || len(text) > maxSymbol {
		return ""
	}
	for _, char := range text {
		if char < 0x21 || char > 0x7e {
			return ""
		}
	}
	return string(text)
}

func trimZeros(raw []byte) []byte {
	end := len(raw)
	for end > 0 && raw[end-1] == 0 {
		end--
	}
	return raw[:end]
}

// wordInt reads a 32-byte big-endian word, or -1 when it exceeds an int.
func wordInt(word []byte) int {
	for _, b := range word[:24] {
		if b != 0 {
			return -1
		}
	}
	value := binary.BigEndian.Uint64(word[24:])
	if value > math.MaxInt32 {
		return -1
	}
	return int(value)
}

// gwei converts wei for a gauge, saturating instead of wrapping.
func gwei(wei *big.Int) int64 {
	if wei == nil || wei.Sign() <= 0 {
		return 0
	}
	value := new(big.Int).Quo(wei, big.NewInt(1e9))
	if !value.IsInt64() {
		return math.MaxInt64
	}
	return value.Int64()
}

type dispatchRecord struct {
	Type string `json:"type"`
	AtMS int64  `json:"at_ms"`
	Lane int    `json:"lane,omitempty"`
	execution.DispatchStatus
}

// publishDispatch writes a lane's dispatch_state record when its nonce,
// budget, halt or stop state changes; the gauges follow the primary lane. It
// runs on the writer goroutine; sends run on the lanes' own.
func (s *reactiveRuntime) publishDispatch(now time.Time, lane *sendLane, write func(any) bool) bool {
	status := lane.dispatcher.Status(now)
	if lane.index != 0 {
		previous := s.laneReported[lane.index]
		if previous != nil && sameDispatch(*previous, status) {
			return true
		}
		s.laneReported[lane.index] = &status
		return write(dispatchRecord{Type: "dispatch_state", AtMS: now.UnixMilli(), Lane: lane.index, DispatchStatus: status})
	}
	flag := func(value bool) int64 {
		if value {
			return 1
		}
		return 0
	}
	m := s.metrics
	m.Set(telemetry.Standard, telemetry.DispatchHalted, flag(status.HaltedReason != ""))
	m.Set(telemetry.Standard, telemetry.DispatchStopped, flag(status.Stopped))
	m.Set(telemetry.Standard, telemetry.DispatchNextNonce, int64(min(status.NextNonce, math.MaxInt64)))
	m.Set(telemetry.Standard, telemetry.DispatchPending, int64(status.Variants))
	m.Set(telemetry.Standard, telemetry.DispatchIncluded, int64(status.Included))
	m.Set(telemetry.Standard, telemetry.DispatchSpent, gwei(status.SpentToday))
	m.Set(telemetry.Standard, telemetry.DispatchDailyCap, gwei(status.DailyCap))
	if previous := s.reported; previous != nil && sameDispatch(*previous, status) {
		return true
	}
	s.reported = &status
	return write(dispatchRecord{Type: "dispatch_state", AtMS: now.UnixMilli(), DispatchStatus: status})
}

func sameDispatch(a, b execution.DispatchStatus) bool {
	samePending := (a.Pending == nil) == (b.Pending == nil) && (a.Pending == nil || *a.Pending == *b.Pending)
	return a.HaltedReason == b.HaltedReason && a.Stopped == b.Stopped && a.NextNonce == b.NextNonce && samePending &&
		a.Variants == b.Variants && a.Included == b.Included && a.SpentToday.Cmp(b.SpentToday) == 0 &&
		a.DailyCap.Cmp(b.DailyCap) == 0 && a.PerTransaction.Cmp(b.PerTransaction) == 0
}

// observeSend counts one dispatcher call. A zero hash means the final check
// refused the intent before anything was signed or broadcast.
func (s *reactiveRuntime) observeSend(job *reactiveJob, action string, hash domain.Hash, lane *sendLane) {
	source := pipeline.MetricSource(job.intent.Version.Source)
	switch {
	case hash == (domain.Hash{}):
		s.metrics.Add(source, telemetry.ReactiveRejected, 1)
		return
	case action == "cancel":
		s.metrics.Add(source, telemetry.ReactiveCancels, 1)
		return
	}
	s.metrics.Add(source, telemetry.ReactiveSent, 1)

	if sent := lane.dispatcher.Submission(hash); sent != nil && sent.ReturnedAtNS >= sent.StartedAtNS {
		if job.timing.ReceivedNS > 0 && sent.StartedAtNS >= job.timing.ReceivedNS {
			s.metrics.Observe(source, telemetry.ReactiveTriggerToSend, time.Duration(sent.StartedAtNS-job.timing.ReceivedNS))
		}
		ack := time.Duration(sent.ReturnedAtNS - sent.StartedAtNS)
		s.metrics.Observe(source, telemetry.ReactiveSendAck, ack)
		if s.settings.planning.Chain.Family == domain.FamilyNitro {
			s.backlog.observe(ack, time.Unix(0, sent.ReturnedAtNS))
		}
	}
}

func (s *reactiveRuntime) observeLanding(source domain.StateSource, observation *execution.ExecutionObservation) {
	if observation.Cancelled {
		return
	}
	metricSource := pipeline.MetricSource(source)
	s.metrics.Add(metricSource, telemetry.ReactiveLanded, 1)
	if observation.SendToObservedNS != nil {
		s.metrics.Observe(metricSource, telemetry.ReactiveSendToFeed, time.Duration(*observation.SendToObservedNS))
	}
}

// publishSettlements writes the dispatcher's booking changes. A journal
// failure is logged; the dispatcher already refuses further sends.
func (s *reactiveRuntime) publishSettlements(updates []execution.SettlementUpdate, err error, write func(any) bool) bool {
	if err != nil {
		slog.Warn("reactive settlement not saved", "error", err)
		return true
	}
	for _, update := range updates {
		if diagnosis := update.Diagnosis; diagnosis != nil {
			if s.preflight != nil {
				s.preflight.diagnosed(diagnosis.Hash, diagnosis.Diagnosis)
			}
			if !write(map[string]any{"type": "revert_diagnosis", "transaction": diagnosis.Hash, "diagnosis": diagnosis.Diagnosis}) {
				return false
			}
			continue
		}
		if update.Voided != (domain.Hash{}) && !write(map[string]any{"type": "settlement_voided", "observed_at_ns": time.Now().UnixNano(), "transaction": update.Voided, "replaced_by": update.Settlement.Hash}) {
			return false
		}
		s.observeSettlement(update)
		if !write(map[string]any{"type": "settlement", "observed_at_ns": time.Now().UnixNano(), "receipt": update.Settlement}) {
			return false
		}
	}
	return true
}

// observeSettlement counts the first booked outcome, and feeds paid fees to
// calibration and the bid model's gas ratio once they are known.
func (s *reactiveRuntime) observeSettlement(update execution.SettlementUpdate) {
	receipt := update.Settlement
	if receipt.Maintenance != "" {
		// A sweep or unwrap moved the wallet; it is no execution sample.
		s.accountStale.Store(true)
		return
	}
	if update.FeesBooked {
		s.research.RecordPaidExecution(receipt)
	}
	if receipt.Cancelled || receipt.Intent == nil {
		return
	}
	if (update.First || update.Voided != (domain.Hash{})) && s.preflight != nil {
		s.preflight.settled(*receipt.Intent, receipt.Hash, receipt.Success, time.Now())
	}
	// Reverts measure partial execution, not the gas priced by a successful bid.
	if update.FeesBooked && s.bids != nil {
		if receipt.Success {
			s.bids.observeGas(receipt.Fees.GasUsed, receipt.Intent.Call.GasLimit)
			s.bids.observeRouteGas(planPools(receipt.Intent.Route), receipt.Intent.Route.Funding, receipt.Fees.GasUsed)
		} else {
			s.bids.observeRevertGas(receipt.Fees.GasUsed, receipt.Intent.Call.GasLimit, receipt.Intent.Conditions != nil)
		}
	}
	if update.FeesBooked && s.bids != nil && receipt.Intent.Bid != nil {
		s.bids.settle(bidOutcomeOf(receipt, time.Now()))
		if s.archive != nil {
			s.archive.outcome(receipt)
		}
	}
	if !update.First {
		return
	}
	source := pipeline.MetricSource(receipt.Intent.Version.Source)
	if receipt.Success {
		s.metrics.Add(source, telemetry.ReactiveSucceeded, 1)
	} else {
		s.metrics.Add(source, telemetry.ReactiveReverted, 1)
	}
}

// pathRecorder writes each fanned-out send's per-path answers next to the
// intent. A full queue drops the record: it is timing evidence, not a nonce
// fact, so it must not stop trading the way a lost observation does.
func (s *reactiveRuntime) pathRecorder(log func(domain.Hash, []execution.PathResult)) func(domain.Hash, []execution.PathResult) {
	if log == nil {
		return nil
	}
	return func(hash domain.Hash, results []execution.PathResult) {
		log(hash, results)
		record := map[string]any{"type": "submission_paths", "transaction": hash, "paths": results, "at_ms": time.Now().UnixMilli()}
		if !s.note(record) {
			slog.Warn("submission path record dropped", "hash", hash.Hex())
		}
	}
}
