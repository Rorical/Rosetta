package remote

import (
	"context"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// sendLane is one sender with its own nonce lane. A conditional send
// holds its lane until the transaction executes, typically about 200 ms; with
// one lane that blocks a measurable share of the opportunity value. Lane 0 is the
// primary sender: s.dispatcher, the native refill, cancellations,
// replacements and wallet-funded routes stay there. An extra lane idles while
// its balance cannot cover a transaction budget.
type sendLane struct {
	index      int
	sender     domain.Address
	dispatcher *execution.Dispatcher // nil in shadow mode
	notify     chan struct{}
	funded     atomic.Bool
}

// primary is lane 0 over the runtime's own dispatcher and job signal.
func (s *reactiveRuntime) primary() *sendLane {
	return &sendLane{sender: s.settings.planning.Sender, dispatcher: s.dispatcher, notify: s.jobs}
}

func (s *reactiveRuntime) allLanes() []*sendLane {
	return append([]*sendLane{s.primary()}, s.extra...)
}

// available reports whether the lane may take a job: the primary always,
// a shadow lane always, an extra live lane once funded.
func (l *sendLane) available() bool {
	return l.index == 0 || l.dispatcher == nil || l.funded.Load()
}

// eligible keeps cancellations and wallet-funded routes on the primary: the
// first targets its pending nonce, the second pulls from its wallet.
func (l *sendLane) eligible(job *reactiveJob) bool {
	return l.index == 0 || job.cancellation == nil && job.intent.Route.Funding != execution.FundingWallet
}

// sendLoop runs one lane's jobs and hands each result to the writer, which
// drains results until every lane has stopped.
func (s *reactiveRuntime) sendLoop(ctx context.Context, stop <-chan struct{}, lane *sendLane) {
	defer s.workers.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		default:
		}
		job := s.takeJob(lane)
		if job == nil {
			select {
			case <-ctx.Done():
				return
			case <-stop:
				return
			case <-lane.notify:
			}
			continue
		}
		result := s.executeJob(ctx, job, lane)
		s.finishJob(job)
		s.results <- result
	}
}

type laneFollowup struct {
	lane     *sendLane
	followup execution.Followup
}

// forwardFollowups merges each lane's follow-up results into one channel for
// the writer, which owns settlement.
func (s *reactiveRuntime) forwardFollowups(stop <-chan struct{}, lanes []*sendLane) <-chan laneFollowup {
	merged := make(chan laneFollowup)
	for _, lane := range lanes {
		if lane.dispatcher == nil {
			continue
		}
		go func() {
			source := lane.dispatcher.Followups()
			for {
				select {
				case <-stop:
					return
				case f := <-source:
					select {
					case merged <- laneFollowup{lane, f}:
					case <-stop:
						return
					}
				}
			}
		}()
	}
	return merged
}

// senders are every lane's sender: their transactions are ours, not rivals'.
func (s *reactiveRuntime) senders() []domain.Address {
	senders := []domain.Address{s.settings.planning.Sender}
	for _, lane := range s.extra {
		senders = append(senders, lane.sender)
	}
	return senders
}

func (s *reactiveRuntime) closeLanes() {
	for _, lane := range s.allLanes() {
		if lane.dispatcher != nil {
			lane.dispatcher.Close()
		}
	}
}

// laneOf returns the lane whose dispatcher watches hash.
func (s *reactiveRuntime) laneOf(hash domain.Hash) (*sendLane, execution.PendingTransaction, bool) {
	for _, lane := range s.allLanes() {
		if lane.dispatcher == nil {
			continue
		}
		if variant, ok := lane.dispatcher.Watched(hash); ok {
			return lane, variant, true
		}
	}
	return nil, execution.PendingTransaction{}, false
}
