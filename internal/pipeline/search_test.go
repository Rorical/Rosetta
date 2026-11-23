package pipeline

import (
	"context"
	"errors"
	"math/big"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

type evaluatorProbe struct {
	current, accepted bool
	submitted         *arb.SearchJob
	err               error
}

func (p *evaluatorProbe) Submit(job arb.SearchJob) error { p.submitted = &job; return p.err }
func (p *evaluatorProbe) Current(arb.SearchEvent) bool   { return p.current }
func (p *evaluatorProbe) Accept(arb.SearchEvent) bool    { return p.accepted }

type cursorFunc func(domain.StateVersion) bool

func (f cursorFunc) Current(v domain.StateVersion) bool { return f(v) }

type sinkFunc func(context.Context, arb.SearchEvent) error

func (f sinkFunc) Consume(ctx context.Context, e arb.SearchEvent) error { return f(ctx, e) }

type expandFunc func(arb.SearchEvent) (arb.SearchJob, bool)

func (f expandFunc) Expand(e arb.SearchEvent) (arb.SearchJob, bool) { return f(e) }

func TestSearchOwnershipAcrossDiscardRetryAndSinkFailure(t *testing.T) {
	for _, scenario := range []string{"stale version", "stale graph", "stale ticket", "suspended", "retry", "rejected retry", "sink failure"} {
		t.Run(scenario, func(t *testing.T) {
			releases, calls := 0, 0
			evaluator := &evaluatorProbe{current: true, accepted: true}
			scheduler := NewSearchScheduler(evaluator, 1, nil)
			event := arb.SearchEvent{Job: arb.SearchJob{GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1}, Release: func() { releases++ }}}
			cursor := cursorFunc(func(domain.StateVersion) bool { return scenario != "stale version" })
			var expander DependencyExpander
			failed := errors.New("sink failure")
			sink := sinkFunc(func(context.Context, arb.SearchEvent) error { calls++; return failed })
			switch scenario {
			case "stale graph":
				event.Job.GraphVersion = 2
			case "stale ticket":
				evaluator.current = false
				event.Err = errors.New("obsolete failure")
			case "suspended":
				scheduler.Suspended = true
			case "retry", "rejected retry":
				expander = expandFunc(func(e arb.SearchEvent) (arb.SearchJob, bool) { return e.Job, true })
				if scenario == "rejected retry" {
					evaluator.err = failed
				}
			}
			err := scheduler.Process(t.Context(), event, cursor, expander, nil, sink)
			switch scenario {
			case "retry":
				if err != nil || releases != 0 || calls != 0 || evaluator.submitted == nil {
					t.Fatal("retry did not transfer ownership", err, releases, calls)
				}
				evaluator.submitted.Release()
			case "rejected retry", "sink failure":
				if !errors.Is(err, failed) || releases != 1 {
					t.Fatal("failure lost error or lease", err, releases)
				}
			default:
				if err != nil || calls != 0 || releases != 1 {
					t.Fatal("stale event reached sink or leaked lease", err, calls, releases)
				}
			}
		})
	}
}

func TestSharedPipelineSearchesPoolViewWithoutNativeExecutionClient(t *testing.T) {
	states := []pool.State{
		{Key: domain.PoolKey{Address: domain.Address{10}, Kind: domain.PoolKindV2}, Token0: domain.Address{1}, Token1: domain.Address{2}, Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(2_000_000), Fee: 3000},
		{Key: domain.PoolKey{Address: domain.Address{20}, Kind: domain.PoolKindV2}, Token0: domain.Address{2}, Token1: domain.Address{1}, Reserve0: big.NewInt(1_000_000), Reserve1: big.NewInt(2_000_000), Fee: 3000},
	}
	topology, err := BuildTopology(t.Context(), states, TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	store := poolstate.New()
	for _, p := range states {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := arb.NewWorker(ctx, topology.Engine, arb.RankedOptions{
		Bounds: func(token domain.Address) (arb.InputBounds, bool) {
			return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100_000)}, token == states[0].Token0
		},
		SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil },
	})
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	scheduler := NewSearchScheduler(worker, 1, nil)
	var released atomic.Int32
	canonical := domain.StateVersion{Source: domain.SourceStandard, Epoch: 1}
	tentative := domain.StateVersion{Source: domain.SourceFast, Epoch: 2}
	for _, version := range []domain.StateVersion{canonical, tentative} {
		if err := scheduler.Submit(arb.SearchJob{View: store.View(), Dirty: []domain.PoolKey{states[0].Key}, GraphVersion: 1, Version: version, Release: func() { released.Add(1) }}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if scheduler.StandardPending != canonical || scheduler.TentativePending != tentative {
		t.Fatal("search lanes share pending cursor")
	}
	for range 2 {
		event := await(t, worker.Results())
		if err := scheduler.Process(ctx, event, cursorFunc(func(domain.StateVersion) bool { return true }), nil, nil, sinkFunc(func(_ context.Context, e arb.SearchEvent) error {
			if len(e.Result.Opportunities) != 1 || e.Result.Opportunities[0].Net.Sign() <= 0 {
				t.Fatal("shared engine lost profitable cycle")
			}
			return nil
		})); err != nil {
			t.Fatal(err)
		}
	}
	if released.Load() != 2 || scheduler.StandardPending != (domain.StateVersion{}) || scheduler.TentativePending != (domain.StateVersion{}) {
		t.Fatal("completed search leaked view or pending cursor")
	}
}

func TestSearchLatencyStartsAtFeedArrivalAndExcludesDiscardedResults(t *testing.T) {
	registry := &telemetry.Registry{}
	evaluator := &evaluatorProbe{current: true, accepted: true}
	scheduler := NewSearchScheduler(evaluator, 1, registry)
	event := arb.SearchEvent{Job: arb.SearchJob{GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}, ReceivedAt: time.Now().Add(-time.Second)}}
	sink := sinkFunc(func(context.Context, arb.SearchEvent) error { return nil })
	for _, current := range []bool{false, true} {
		if err := scheduler.Process(t.Context(), event, cursorFunc(func(domain.StateVersion) bool { return current }), nil, nil, sink); err != nil {
			t.Fatal(err)
		}
	}
	latency := registry.Snapshot().Sources["fast"].Stages["candidate_search"]
	if latency.Count != 1 || latency.SumNS < uint64(time.Second) {
		t.Fatal("feed waiting time lost or stale result measured", latency)
	}
}

func TestSearchCountsWindowAndCapacityLimits(t *testing.T) {
	registry := &telemetry.Registry{}
	scheduler := NewSearchScheduler(&evaluatorProbe{current: true, accepted: true}, 1, registry)
	event := arb.SearchEvent{Job: arb.SearchJob{GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}},
		Result: arb.SearchResult{Limits: arb.QuoteLimits{OutOfWindow: 2, Capacity: 7}}}
	sink := sinkFunc(func(context.Context, arb.SearchEvent) error { return nil })
	if err := scheduler.Process(t.Context(), event, cursorFunc(func(domain.StateVersion) bool { return true }), nil, nil, sink); err != nil {
		t.Fatal(err)
	}
	counters := registry.Snapshot().Sources["fast"].Counters
	if counters["quote_out_of_window"] != 2 || counters["quote_capacity"] != 7 {
		t.Fatal("quote limits not exported", counters)
	}
}

func TestIncrementalSchedulerCarriesPartialToCurrentPrefixAndFinishesSeparately(t *testing.T) {
	states := []pool.State{
		{Key: domain.PoolKey{Address: domain.Address{10}, Kind: domain.PoolKindV2}, Token0: domain.Address{1}, Token1: domain.Address{2}, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(2000000), Fee: 3000},
		{Key: domain.PoolKey{Address: domain.Address{20}, Kind: domain.PoolKindV2}, Token0: domain.Address{2}, Token1: domain.Address{1}, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(2000000), Fee: 3000},
	}
	topology, err := BuildTopology(t.Context(), states, TopologyOptions{Depth: 2, MaxCycles: 100, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
	if err != nil {
		t.Fatal(err)
	}
	store := poolstate.New()
	for _, p := range states {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	w, err := arb.NewWorker(ctx, topology.Engine, arb.RankedOptions{Bounds: func(token domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, token == states[0].Token0
	}, SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-w.Done() })
	registry := &telemetry.Registry{}
	s := NewSearchScheduler(w, 1, registry)
	version := domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 10}
	job := arb.SearchJob{Incremental: true, View: store.View(), Dirty: []domain.PoolKey{states[0].Key}, Version: version, GraphVersion: 1, ReceivedAt: time.Now()}
	var released atomic.Int32
	job.Release = func() { released.Add(1) }
	if err := s.Submit(job, nil); err != nil {
		t.Fatal(err)
	}
	partial := await(t, w.Results())
	if !partial.Partial {
		t.Fatal("candidate did not stream")
	}
	version.TxIndex = 1
	next := job
	next.Version = version
	next.Dirty = nil
	next.Recheck = job.Dirty
	next.Release = nil
	if err := s.Submit(next, nil); err != nil {
		t.Fatal(err)
	}
	candidates := 0
	consume := sinkFunc(func(_ context.Context, event arb.SearchEvent) error {
		if event.Job.Version != version {
			t.Fatal("carried quote retained obsolete prefix")
		}
		candidates += len(event.Result.Opportunities)
		return nil
	})
	cursor := cursorFunc(func(v domain.StateVersion) bool { return v == version })
	if err := s.Process(ctx, partial, cursor, nil, nil, consume); err != nil {
		t.Fatal(err)
	}
	if s.TentativePending == (domain.StateVersion{}) || released.Load() != 0 {
		t.Fatal("partial result closed the whole batch")
	}
	for count := 0; count < 6 && s.TentativePending != (domain.StateVersion{}); count++ {
		nextEvent := await(t, w.Results())
		if err := s.Process(ctx, nextEvent, cursor, nil, nil, consume); err != nil {
			t.Fatal(err)
		}
	}
	if candidates != 1 || released.Load() != 1 || s.TentativePending != (domain.StateVersion{}) {
		t.Fatal("stream duplicated candidate or leaked ownership", candidates, released.Load())
	}
	metrics := registry.Snapshot().Sources["fast"]
	if metrics.Counters["candidates_streamed"] != 1 || metrics.Stages["first_candidate"].Count != 1 || metrics.Stages["candidate_search"].Count != 2 || metrics.Counters["candidates_duplicate"] != 1 {
		t.Fatal("partial/final latency counters mixed", metrics)
	}
}
