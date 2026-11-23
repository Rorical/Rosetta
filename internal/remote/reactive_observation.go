package remote

import (
	"fmt"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func (r *runtime) observeReactiveExecution(hash domain.Hash, success bool, logs []feed.FastLog, position execution.ExecutionPosition) {
	s := r.reactive
	if s == nil || s.dispatcher == nil {
		return
	}
	// Most feed transactions are unrelated. Avoid converting their logs.
	lane, variant, ok := s.laneOf(hash)
	if !ok {
		return
	}
	receiptLogs := make([]rpc.Log, len(logs))
	for i, log := range logs {
		receiptLogs[i] = rpc.Log{Address: log.Address, Topics: log.Topics, Data: log.Data}
	}
	observation := lane.dispatcher.ObserveExecution(hash, success, receiptLogs, position)
	if observation == nil {
		return
	}
	s.own.seen(lane.index, observation.Nonce, position.BlockNumber, position.TransactionIndex)
	if variant.Maintenance != nil {
		s.enqueueObservation(observation)
		return
	}
	if old := s.lastObserved; old != nil && old.Hash == hash &&
		old.Position.Stream == position.Stream && old.Position.PayloadID == position.PayloadID &&
		old.Position.BlockNumber == position.BlockNumber && old.Position.FrameIndex == position.FrameIndex && old.Position.TransactionIndex == position.TransactionIndex {
		return
	}
	s.lastObserved = observation
	if !success && variant.Cancellation == nil {
		s.own.reverted(routePools(variant.Intent), variant.Intent.Version, position.BlockNumber, position.TransactionIndex, time.Now())
	}
	s.observeLanding(variant.Intent.Version.Source, observation)
	s.enqueueObservation(observation)
}

// Feed processing never waits for journal fsync. An overflow stops new sends
// rather than silently losing execution evidence while continuing to trade.
func (s *reactiveRuntime) enqueueObservation(value any) {
	select {
	case s.observations <- value:
	default:
		err := fmt.Errorf("reactive execution observation queue full")
		s.failure.Store(&err)
	}
}

// note queues a diagnostic record and reports whether it was kept. Diagnostics
// have their own queue: the writer also sends, and while it waits seconds on a
// slow sequencer, bid_declined records could fill a shared queue and a full
// queue stops the runtime at the next execution observation.
func (s *reactiveRuntime) note(record any) bool {
	select {
	case s.diagnostics <- record:
		return true
	default:
		return false
	}
}

func (r *runtime) invalidateReactiveObservation(reason string, fromBlock uint64) {
	s := r.reactive
	if s == nil || s.lastObserved == nil || s.lastObserved.Position.BlockNumber < fromBlock {
		return
	}
	s.enqueueObservation(map[string]any{"type": "execution_observation_invalidated", "transaction": s.lastObserved.Hash, "position": s.lastObserved.Position, "reason": reason, "observed_at_ns": time.Now().UnixNano()})
	s.lastObserved = nil
	// A booked outcome stands: losing a stream position does not prove that an
	// executed transaction disappeared. The canonical follower checks that.
}

// observeReactiveCanonical hands the dispatcher our receipts from a block the
// canonical follower already read, or the gap a follower reset leaves.
func (r *runtime) observeReactiveCanonical(update canonicalUpdate) {
	if r.reactive == nil {
		return
	}
	// Lender balances follow every canonical block, shadow included.
	if update.reset != nil {
		r.reactive.lenders.reset()
	} else {
		r.reactive.lenders.setLenders(r.flashLenders())
		r.reactive.lenders.observe(update.block.Ref, update.transactions)
	}
	if r.reactive.dispatcher == nil {
		return
	}
	for _, lane := range r.reactive.allLanes() {
		d := lane.dispatcher
		if update.reset != nil {
			r.reactive.accountStale.Store(true)
			d.ObserveCanonicalGap()
			continue
		}
		var ours []execution.CanonicalReceipt
		for _, tx := range update.transactions {
			if _, ok := d.Watched(tx.Hash); !ok {
				continue
			}
			logs := make([]rpc.Log, len(tx.Logs))
			for i, log := range tx.Logs {
				logs[i] = rpc.Log{Address: log.Address, Topics: log.Topics, Data: log.Data}
			}
			ours = append(ours, execution.CanonicalReceipt{Hash: tx.Hash, Success: tx.Successful, Logs: logs, Fees: tx.Fees})
		}
		if len(ours) > 0 {
			r.reactive.accountStale.Store(true)
		}
		d.ObserveCanonical(update.block.Ref, ours)
	}
}
