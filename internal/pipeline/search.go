package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// SearchEvaluator owns evaluation tickets and dirty-set accumulation. arb.Worker
// implements this port; the scheduler does not depend on an execution client.
type SearchEvaluator interface {
	Submit(arb.SearchJob) error
	Current(arb.SearchEvent) bool
	Accept(arb.SearchEvent) bool
}

// VersionCursor is read only on the state writer. Tentative freshness can use
// stream counters or a pending-state generation rather than a canonical hash.
type VersionCursor interface {
	Current(domain.StateVersion) bool
}
type ConfirmationLifecycle interface {
	Advance(domain.StateVersion, uint64)
}
type CandidateSink interface {
	Consume(context.Context, arb.SearchEvent) error
}

// DependencyExpander requests a bounded retry against the same snapshot. A
// successful retry must preserve Job.Release; ownership transfers to Submit.
type DependencyExpander interface {
	Expand(arb.SearchEvent) (arb.SearchJob, bool)
}

// SearchScheduler is driven serially by its backend's state writer. Evaluation
// runs independently. Canonical and tentative pending positions remain distinct.
type SearchScheduler struct {
	// Observe records completed/canceled work independently of result acceptance.
	Observe          func(arb.SearchJob, arb.SearchObservation)
	publications     [2]candidatePublications
	evaluator        SearchEvaluator
	metrics          *telemetry.Registry
	GraphVersion     uint64
	StandardPending  domain.StateVersion
	TentativePending domain.StateVersion
	Suspended        bool
}

func NewSearchScheduler(evaluator SearchEvaluator, graph uint64, metrics *telemetry.Registry) *SearchScheduler {
	return &SearchScheduler{evaluator: evaluator, GraphVersion: graph, metrics: metrics}
}

func (s *SearchScheduler) Submit(job arb.SearchJob, lifecycle ConfirmationLifecycle) error {
	if s.Suspended {
		releaseSearch(job)
		return nil
	}
	source := MetricSource(job.Version.Source)
	job.Observe = s.observer(job)
	s.metrics.Add(source, telemetry.SearchSubmitted, 1)
	if lifecycle != nil {
		lifecycle.Advance(job.Version, job.GraphVersion)
	}
	if job.Version.Source != domain.SourceFast && (len(job.Dirty)+len(job.Recheck) > 0 || s.StandardPending != (domain.StateVersion{})) {
		s.StandardPending = job.Version
	}
	if job.Version.Source == domain.SourceFast && (len(job.Dirty)+len(job.Recheck) > 0 || s.TentativePending != (domain.StateVersion{})) {
		s.TentativePending = job.Version
	}
	err := s.evaluator.Submit(job)
	if err != nil {
		releaseSearch(job)
	}
	return err
}

// Process consumes every delivered event, including stale results. It either
// releases its reader after sinks return, or transfers it to a dependency retry.
func (s *SearchScheduler) Process(ctx context.Context, event arb.SearchEvent, cursor VersionCursor, expander DependencyExpander, lifecycle ConfirmationLifecycle, sink CandidateSink) error {
	defer func() { releaseSearch(event.Job) }()
	source := MetricSource(event.Job.Version.Source)
	if rebaser, ok := s.evaluator.(interface{ Rebase(*arb.SearchEvent) bool }); ok && !rebaser.Rebase(&event) {
		s.metrics.Add(source, telemetry.SearchDiscarded, 1)
		return nil
	}
	if s.Suspended || event.Job.GraphVersion != s.GraphVersion || !cursor.Current(event.Job.Version) || !s.evaluator.Current(event) {
		// Rebase passed, so the candidate's own pools are unchanged: it is
		// dropped for queued input or a moved cursor. The evaluator has
		// acknowledged its pools, so the route goes back to be searched again
		// with the next frame.
		if rechecker, ok := s.evaluator.(interface{ Recheck(arb.SearchEvent) bool }); ok && rechecker.Recheck(event) {
			s.metrics.Add(source, telemetry.CandidatesRechecked, 1)
		}
		s.metrics.Add(source, telemetry.SearchDiscarded, 1)
		return nil
	}
	if event.Err != nil {
		return fmt.Errorf("candidate search: %w", event.Err)
	}
	if !event.Partial && expander != nil {
		if job, retry := expander.Expand(event); retry {
			if event.Job.Incremental {
				retryEvaluator, ok := s.evaluator.(interface{ Retry(arb.SearchJob) error })
				if !ok {
					return fmt.Errorf("incremental evaluator cannot retry dependencies")
				}
				job.Observe = s.observer(job)
				if err := retryEvaluator.Retry(job); err != nil {
					return err
				}
				event.Job.Release = nil
				return nil
			}
			if !s.evaluator.Accept(event) {
				s.metrics.Add(source, telemetry.SearchDiscarded, 1)
				return nil
			}
			event.Job.Release = nil
			return s.Submit(job, lifecycle)
		}
	}
	if !s.evaluator.Accept(event) {
		s.metrics.Add(source, telemetry.SearchDiscarded, 1)
		return nil
	}
	if event.Partial && !s.freshCandidate(event) {
		s.metrics.Add(source, telemetry.CandidatesDuplicate, 1)
		return nil
	}
	if !event.Partial {
		pending := false
		if evaluator, ok := s.evaluator.(interface{ PendingAfter(arb.SearchEvent) bool }); ok {
			pending = evaluator.PendingAfter(event)
		}
		if !pending {
			if event.Job.Version.Source == domain.SourceFast {
				s.TentativePending = domain.StateVersion{}
			} else {
				s.StandardPending = domain.StateVersion{}
			}
		}
	}
	for _, issue := range event.Result.Issues {
		// An empty pool needs no refresh. Arbitrum One's WBTC/USDC 0x6fe8…584d
		// holds 88 units of liquidity and logged this four times a second.
		if errors.Is(issue.Err, pool.ErrNoLiquidity) {
			continue
		}
		slog.Warn("candidate quote needs dependency refresh", "pool", issue.Pool.Address.Hex(), "error", issue.Err)
	}
	s.metrics.Add(source, telemetry.PathsConsidered, uint64(event.Result.Considered))
	s.metrics.Add(source, telemetry.PathsSelected, uint64(event.Result.Selected))
	s.metrics.Add(source, telemetry.QuoteOutOfWindow, uint64(event.Result.Limits.OutOfWindow))
	s.metrics.Add(source, telemetry.QuoteCapacity, uint64(event.Result.Limits.Capacity))
	s.metrics.Add(source, telemetry.Candidates, uint64(len(event.Result.Opportunities)))
	s.metrics.Add(source, telemetry.ExploreSteps, uint64(event.Result.Exploration.Steps))
	s.metrics.Add(source, telemetry.ExplorePaths, uint64(event.Result.Exploration.Discovered))
	if event.Result.Exploration.Truncated {
		s.metrics.Add(source, telemetry.ExploreTruncated, 1)
	}
	if event.Result.Exploration.BudgetExhausted {
		s.metrics.Add(source, telemetry.ExploreBudgetExhausted, 1)
	}
	if event.Result.Exploration.CacheFull {
		s.metrics.Add(source, telemetry.ExploreCacheFull, 1)
	}
	if event.Result.Exploration.CatalogPaths > 0 {
		s.metrics.Set(source, telemetry.ExploreCatalogPaths, int64(event.Result.Exploration.CatalogPaths))
	}
	if err := sink.Consume(ctx, event); err != nil {
		return err
	}
	if event.Partial {
		s.metrics.Add(source, telemetry.CandidatesStreamed, 1)
		if event.MarkFirstCandidate() {
			s.metrics.Since(source, telemetry.FirstCandidate, event.Job.ReceivedAt)
		}
		if event.MarkFirstShortCandidate() {
			s.metrics.Since(source, telemetry.FirstShortCandidate, event.Job.FirstReceivedAt)
		}
		return nil
	}
	s.metrics.Since(source, telemetry.CandidateSearch, event.Job.ReceivedAt)
	published := len(event.Result.Opportunities)
	if event.Job.Incremental {
		published = event.Result.Published
	}
	slog.Info("candidate search completed", "source", event.Job.Version.Source, "block", event.Job.Version.BlockNumber, "anchor_block", event.Job.Version.Anchor.Number, "index", event.Job.Version.TxIndex,
		"considered", event.Result.Considered, "selected", event.Result.Selected, "probe_only", event.Result.ProbeOnly, "candidates", published, "explore_steps", event.Result.Exploration.Steps, "paths_discovered", event.Result.Exploration.Discovered, "cached_paths", event.Result.Exploration.Cached, "catalog_paths", event.Result.Exploration.CatalogPaths, "explore_truncated", event.Result.Exploration.Truncated)
	return nil
}

func releaseSearch(job arb.SearchJob) {
	if job.Release != nil {
		job.Release()
	}
}
