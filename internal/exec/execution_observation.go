package exec

import (
	"context"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// SubmissionTiming measures our send call, never the sequencer's internal clock.
// Timing is observational; the durable pending journal remains nonce authority.
type SubmissionTiming struct {
	Hash         domain.Hash
	StartedAtNS  int64
	ReturnedAtNS int64 `json:",omitempty"`
	Acknowledged bool
}

func (d *Dispatcher) send(ctx context.Context, hash domain.Hash, raw []byte, conditions *TransactionConditions) (domain.Hash, error) {
	timing := SubmissionTiming{Hash: hash, StartedAtNS: time.Now().UnixNano()}
	d.submissions.Store(hash, timing)
	got, err := d.network.Send(ctx, raw, conditions)
	timing.ReturnedAtNS = time.Now().UnixNano()
	timing.Acknowledged = err == nil && got == hash
	d.submissions.Store(hash, timing)
	return got, err
}

func (d *Dispatcher) Submission(hash domain.Hash) *SubmissionTiming {
	value, ok := d.submissions.Load(hash)
	if !ok {
		return nil
	}
	timing := value.(SubmissionTiming)
	return &timing
}

// ExecutionPosition names a feed position without inventing a final block hash.
type ExecutionPosition struct {
	Stream                                    string
	PayloadID                                 string `json:",omitempty"`
	BlockNumber, FrameIndex, TransactionIndex uint64
	ReceivedAtNS                              int64
}

// ExecutionObservation is feed execution evidence. SettleObserved books it;
// paid network fees and net profit come later from a receipt.
type ExecutionObservation struct {
	Type                        string `json:"type"`
	Chain                       domain.ChainIdentity
	Hash, IntentID              domain.Hash
	Nonce                       uint64
	Maintenance                 string `json:",omitempty"`
	Position                    ExecutionPosition
	Submission                  *SubmissionTiming `json:",omitempty"`
	SendToObservedNS            *int64            `json:",omitempty"`
	Success, Cancelled          bool
	Asset                       domain.Address
	Profit, LoanFee, QuoteError *big.Int `json:",omitempty"`
	Error                       string   `json:",omitempty"`
}

// ObserveExecution matches every variant of every unfinished nonce, including
// cancellations, without waiting for the sender's mutex or making RPC calls.
func (d *Dispatcher) ObserveExecution(hash domain.Hash, success bool, logs []rpc.Log, position ExecutionPosition) *ExecutionObservation {
	variant, ok := d.Watched(hash)
	if !ok {
		return nil
	}
	matched := &variant
	d.observeNonce(matched.Nonce)
	result := &ExecutionObservation{Type: "execution_observed", Chain: matched.Intent.Chain, Hash: hash, IntentID: matched.Intent.ID, Nonce: matched.Nonce, Position: position, Submission: d.Submission(hash), Success: success, Cancelled: matched.Cancellation != nil, Asset: matched.Intent.Route.Asset}
	if result.Submission != nil && position.ReceivedAtNS >= result.Submission.StartedAtNS {
		latency := position.ReceivedAtNS - result.Submission.StartedAtNS
		result.SendToObservedNS = &latency
	}
	if matched.Maintenance != nil {
		result.Maintenance = matched.Maintenance.Kind
		return result
	}
	if success && !result.Cancelled {
		profit, loan, err := executorOutcome(*matched, logs)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Profit, result.LoanFee = profit, loan
		result.QuoteError = new(big.Int).Add(profit, loan)
		result.QuoteError.Sub(result.QuoteError, matched.Intent.QuotedGross)
	}
	return result
}
