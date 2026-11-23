package arb

import (
	"context"
	"encoding/json"
	"math/big"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type refinementGate struct {
	inner           Quoter
	key             domain.PoolKey
	calls           atomic.Int64
	entered, finish chan struct{}
}

func (q *refinementGate) QuoteExactIn(ctx context.Context, p pool.State, deep pool.DeepState, token domain.Address, amount *big.Int) (*big.Int, error) {
	if p.Key == q.key && q.calls.Add(1) == 2 {
		close(q.entered)
		select {
		case <-q.finish:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return q.inner.QuoteExactIn(ctx, p, deep, token, amount)
}

func TestInitialQuotePublishesBeforeSizingAndOwnsItsBuffers(t *testing.T) {
	pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000)}
	q := &refinementGate{inner: v2CurveQuoter(), key: pools[0].Key, entered: make(chan struct{}), finish: make(chan struct{})}
	engine := mustEngine(t, explicitTable(t, pools[0].Token0, []domain.PoolKey{pools[0].Key, pools[1].Key}), 1000, 1, q)
	opts := rankedOptions(pools[0].Token0, 100000)
	opts.StreamInitial = true
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	view := newFakeView()
	for _, p := range pools {
		view.add(p)
	}
	job := SearchJob{Incremental: true, View: view, Dirty: []domain.PoolKey{pools[0].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}, ReceivedAt: time.Now()}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	initial := nextSearch(t, worker)
	waitSignal(t, q.entered)
	if initial.Stage != QuoteInitial || !initial.First || initial.QuotedAt.Before(job.ReceivedAt) || !worker.Accept(initial) {
		t.Fatal("first quote did not precede refinement", initial.Stage)
	}
	stages := initial.Stages
	accounted := stages.Intake + stages.Queue + stages.Catalog + stages.Prepare + stages.RankCPU + stages.Rank + stages.ExactPrepare + stages.Snapshot + stages.PathQueue + stages.PathCPU + stages.Math
	if stages.Math <= 0 || stages.Rank <= 0 || accounted > initial.QuotedAt.Sub(job.ReceivedAt) {
		t.Fatal("overlapping or absent first-quote phases", stages)
	}
	original := copyOpportunity(initial.Result.Opportunities[0])
	close(q.finish)
	optimized := nextSearch(t, worker)
	if optimized.Stage != QuoteOptimized || optimized.First || !worker.Accept(optimized) {
		t.Fatal("optimized quote was lost")
	}
	if !reflect.DeepEqual(initial.Result.Opportunities[0], original) {
		t.Fatal("refinement mutated delivered initial quote")
	}
	if optimized.Result.Opportunities[0].Net.Cmp(original.Net) < 0 {
		t.Fatal("refinement lost positive quote")
	}
	releaseSearch(initial.Job)
	releaseSearch(optimized.Job)
	final := nextSearch(t, worker)
	if final.Result.Published != 2 || !worker.Accept(final) {
		t.Fatal("publication accounting", final.Result.Published)
	}
	releaseSearch(final.Job)
}

func TestRecordedInitialQuoteKeepsFinalSizing(t *testing.T) {
	engine, view, dirty, opts := recordedSearchFixture(t)
	selection, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	positives := 0
	for _, candidate := range selection.candidates {
		want, _, ok, err := engine.optimizeCycle(t.Context(), view, candidate)
		if err != nil {
			t.Fatal(err)
		}
		seen := 0
		got, _, gotOK, err := engine.optimizeCycleObserved(t.Context(), view, candidate, func(quote Opportunity) error {
			if quote.Net.Sign() <= 0 || len(quote.Legs) != len(candidate.cycle.Pools) {
				t.Fatal("incomplete initial route")
			}
			seen++
			return nil
		}, &QuoteLimits{})
		if err != nil || ok != gotOK || !reflect.DeepEqual(got, want) {
			t.Fatal("streaming changed final sizing", err)
		}
		if ok && want.Net.Sign() > 0 {
			positives++
			if seen != 1 {
				t.Fatal("missing initial positive", seen)
			}
		}
	}
	if positives == 0 {
		t.Fatal("recorded fixture produced no positive paths")
	}
}

// A single compute slot makes starvation deterministic: refinement of the first
// path must not prevent another selected path from publishing its model quote.
func TestInitialSweepVisitsOtherPathsBeforeRefinement(t *testing.T) {
	pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000), testV2State(3, 2, 1, 1000000, 2100000)}
	q := &refinementGate{inner: v2CurveQuoter(), key: pools[1].Key, entered: make(chan struct{}), finish: make(chan struct{})}
	table := explicitTable(t, pools[0].Token0, []domain.PoolKey{pools[0].Key, pools[1].Key}, []domain.PoolKey{pools[0].Key, pools[2].Key})
	engine := mustEngine(t, table, 1000, 2, q)
	opts := rankedOptions(pools[0].Token0, 100000)
	opts.StreamInitial = true
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	worker.cpus = newSearchCPUsOf(1)
	t.Cleanup(func() { cancel(); <-worker.Done() })
	view := newFakeView()
	for _, p := range pools {
		view.add(p)
	}
	if err := worker.Submit(SearchJob{Incremental: true, View: view, Dirty: []domain.PoolKey{pools[0].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		event := nextSearch(t, worker)
		if event.Stage != QuoteInitial || !worker.Accept(event) {
			t.Fatal("refinement overtook initial sweep", event.Stage)
		}
		releaseSearch(event.Job)
	}
	waitSignal(t, q.entered)
	close(q.finish)
	for {
		event := nextSearch(t, worker)
		if !worker.Accept(event) {
			t.Fatal("result rejected")
		}
		releaseSearch(event.Job)
		if !event.Partial {
			if event.Result.Published != 4 {
				t.Fatal("publication count", event.Result.Published)
			}
			break
		}
	}
}

func TestInitialSweepPreservesRecordedOptimizedResults(t *testing.T) {
	engine, view, dirty, opts := recordedSearchFixture(t)
	opts.StreamInitial = true
	opts.Diverse = true
	selection, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := engine.Optimize(t.Context(), view, selection)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	if err = worker.Submit(SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}); err != nil {
		t.Fatal(err)
	}
	var optimized []Opportunity
	for {
		event := nextSearch(t, worker)
		if event.Err != nil || !worker.Accept(event) {
			t.Fatal("sweep result", event.Err)
		}
		if event.Stage == QuoteOptimized {
			optimized = append(optimized, event.Result.Opportunities...)
		}
		releaseSearch(event.Job)
		if !event.Partial {
			break
		}
	}
	encode := func(quotes []Opportunity) []string {
		var out []string
		for _, q := range quotes {
			raw, err := json.Marshal(q)
			if err != nil {
				t.Fatal(err)
			}
			out = append(out, string(raw))
		}
		slices.Sort(out)
		return out
	}
	if !reflect.DeepEqual(encode(optimized), encode(expected.Opportunities)) {
		t.Fatal("initial sweep changed final recorded quotes")
	}
}
