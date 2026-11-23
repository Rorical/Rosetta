package arb

import (
	"context"
	"math/big"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestNewDirtyFramePreemptsCoarsePreparation(t *testing.T) {
	tests := []struct {
		name   string
		source domain.StateSource
		exact  bool
	}{
		{"standard/shallow", domain.SourceStandardPrefix, false},
		{"fast/shallow", domain.SourceFast, false},
		{"standard/exact", domain.SourceStandardPrefix, true},
		{"fast/exact", domain.SourceFast, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000)}
			engine := mustEngine(t, explicitTable(t, pools[0].Token0, []domain.PoolKey{pools[0].Key, pools[1].Key}), 1000, 1, v2CurveQuoter())
			view := newFakeView()
			for _, p := range pools {
				view.add(p)
			}
			ctx, cancel := context.WithCancel(t.Context())
			worker, err := NewWorker(ctx, engine, rankedOptions(pools[0].Token0, 100000))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cancel(); <-worker.Done() })
			entered, canceled, finish, newer := make(chan struct{}), make(chan struct{}), make(chan struct{}), make(chan struct{})
			var released atomic.Bool
			job := SearchJob{Incremental: true, View: view, Dirty: []domain.PoolKey{pools[0].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: test.source, Epoch: 1}, Release: func() { released.Store(true) }}
			prepare := func(ctx context.Context, v View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
				close(entered)
				<-ctx.Done()
				close(canceled)
				<-finish
				return nil, nil, ctx.Err()
			}
			if test.exact {
				job.PrepareExact = prepare
			} else {
				job.Prepare = prepare
			}
			if err := worker.Submit(job); err != nil {
				t.Fatal(err)
			}
			waitSignal(t, entered)
			heartbeat := job
			heartbeat.Release = nil
			heartbeat.Prepare = nil
			heartbeat.PrepareExact = nil
			heartbeat.Dirty = nil
			heartbeat.Version.TxIndex++
			if err := worker.Submit(heartbeat); err != nil {
				t.Fatal(err)
			}
			select {
			case <-canceled:
				t.Fatal("heartbeat canceled active preparation")
			default:
			}
			// The frame changes the trigger pool, so every path the discovery
			// could rank is obsolete.
			next := heartbeat
			next.Version.TxIndex++
			next.Dirty = []domain.PoolKey{pools[0].Key, pools[1].Key}
			next.Prepare = func(_ context.Context, v View, keys []domain.PoolKey) (View, []QuoteIssue, error) {
				if len(keys) != 2 {
					t.Error("lost accumulated dirty pools", keys)
				}
				close(newer)
				return v, nil, nil
			}
			if err := worker.Submit(next); err != nil {
				t.Fatal(err)
			}
			select {
			case <-canceled:
			case <-time.After(time.Second):
				close(finish)
				t.Fatal("new frame waited for obsolete coarse work")
			}
			if released.Load() {
				t.Fatal("reader released before preparation exited")
			}
			close(finish)
			waitSignal(t, newer)
			event := nextSearch(t, worker)
			releaseSearch(event.Job)
			if !released.Load() {
				t.Fatal("obsolete reader retained after cancellation")
			}
		})
	}
}

func TestShortRouteQuotesBeforeHigherLongVariants(t *testing.T) {
	for _, compiled := range []bool{false, true} {
		t.Run(map[bool]string{false: "generic", true: "compiled"}[compiled], func(t *testing.T) {
			pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 3, 1000000, 2000000), testV2State(3, 3, 1, 1000000, 2000000), testV2State(4, 3, 4, 1000000, 2000000), testV2State(5, 4, 5, 1000000, 2000000), testV2State(6, 5, 6, 1000000, 2000000), testV2State(7, 6, 1, 1000000, 2000000)}
			short := []domain.PoolKey{pools[0].Key, pools[1].Key, pools[2].Key}
			long := []domain.PoolKey{pools[0].Key, pools[1].Key, pools[3].Key, pools[4].Key, pools[5].Key, pools[6].Key}
			engine := mustEngine(t, explicitTable(t, pools[0].Token0, long, short), 1000, 1, v2CurveQuoter())
			if compiled {
				if err := engine.PreparePaths(t.Context(), pools); err != nil {
					t.Fatal(err)
				}
			}
			view := newFakeView()
			for _, p := range pools {
				view.add(p)
			}
			opts := rankedOptions(pools[0].Token0, 100000)
			opts.StreamInitial = true
			opts.Diverse = true
			reference, err := engine.EvaluateRanked(t.Context(), view, []domain.PoolKey{pools[0].Key}, opts)
			if err != nil || len(reference.Opportunities) != 1 || !slices.Equal(reference.Opportunities[0].Cycle.Pools, long) {
				t.Fatal("fixture must exclude short route from final K", reference, err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			worker, err := NewWorker(ctx, engine, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cancel(); <-worker.Done() })

			prepared := map[domain.PoolKey]int{}
			var previous View = view
			prepare := func(_ context.Context, current View, keys []domain.PoolKey) (View, []QuoteIssue, error) {
				if current != previous {
					t.Error("preparation discarded the prior prepared view")
				}
				for _, key := range keys {
					prepared[key]++
					if prepared[key] != 1 {
						t.Error("repeated exact dependency request", key)
					}
				}
				next := newFakeView()
				for _, p := range pools {
					next.add(p)
				}
				previous = next
				return next, nil, nil
			}
			if err := worker.Submit(SearchJob{Incremental: true, View: view, PrepareExact: prepare, Dirty: []domain.PoolKey{pools[0].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}); err != nil {
				t.Fatal(err)
			}
			published, optimized := 0, 0
			var shortInitial, shortOptimized *big.Int
			for {
				event := nextSearch(t, worker)
				if event.Err != nil {
					t.Fatal(event.Err)
				}
				worker.Accept(event)
				if !event.Partial {
					releaseSearch(event.Job)
					if event.Result.Published != published {
						t.Fatal("incorrect streamed accounting", event.Result.Published, published)
					}
					break
				}
				published++
				q := event.Result.Opportunities[0]
				if slices.Equal(q.Cycle.Pools, short) {
					if event.Stage == QuoteInitial {
						shortInitial = new(big.Int).Set(q.Net)
					}
					if event.Stage == QuoteOptimized {
						shortOptimized = new(big.Int).Set(q.Net)
					}
				}
				if event.Stage == QuoteOptimized {
					optimized++
					if event.Stages.Math <= 0 {
						t.Fatal("missing math timing")
					}
					if slices.Equal(q.Cycle.Pools, long) && q.Net.Cmp(reference.Opportunities[0].Net) != 0 {
						t.Fatal("changed final sizing")
					}
				}
				releaseSearch(event.Job)
			}
			if shortInitial == nil || shortOptimized == nil || shortOptimized.Cmp(shortInitial) < 0 {
				t.Fatal("lost early representative outside final K")
			}

			if len(prepared) != len(pools) {
				t.Fatal("missing selected exact dependencies", prepared)
			}
			if optimized != 2 {
				t.Fatal("lost early or final selected route", optimized)
			}
		})
	}
}
