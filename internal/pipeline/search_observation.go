package pipeline

import (
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
)

type prefixGapCounter struct {
	mu          sync.Mutex
	count       uint64
	first, last int64
}

func (c *prefixGapCounter) add(at int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.count == 0 || at < c.first {
		c.first = at
	}
	c.last = max(c.last, at)
	c.count++
}
func (c *prefixGapCounter) take() (count uint64, first, last int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	count, first, last = c.count, c.first, c.last
	c.count, c.first, c.last = 0, 0, 0
	return
}
func (r *Research) flushTraceGaps(emit func(ResearchEvent) error, at time.Time) error {
	if count, first, last := r.prefixGaps.take(); count > 0 {
		if err := emit(ResearchEvent{Type: "prefix_gap", AtMS: at.UnixMilli(), Skipped: count, Censored: true, Details: struct {
			First int64 `json:"first_received_at_ns"`
			Last  int64 `json:"last_received_at_ns"`
		}{first, last}}); err != nil {
			return err
		}
	}
	if count := r.searchSkipped.Swap(0); count > 0 {
		return emit(ResearchEvent{Type: "search_gap", AtMS: at.UnixMilli(), Skipped: count, Censored: true})
	}
	return nil
}

// ObserveSearch is nonblocking and retains no pool views or readers. Canceled
// and superseded work remains visible even when its results cannot be accepted.
func (r *Research) ObserveSearch(job arb.SearchJob, observation arb.SearchObservation) {
	if r == nil {
		return
	}
	event := ResearchEvent{Type: "search", AtMS: time.Now().UnixMilli(), Version: job.Version, Graph: job.GraphVersion, Details: struct {
		CompletedAtNS int64                 `json:"completed_at_ns"`
		ReceivedAtNS  int64                 `json:"received_at_ns"`
		Observation   arb.SearchObservation `json:"observation"`
	}{time.Now().UnixNano(), job.ReceivedAt.UnixNano(), observation}}
	select {
	case r.confirmations <- event:
	default:
		r.searchSkipped.Add(1)
	}
}
