package arb

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestBackgroundPathsCoverQuietMarketsWithoutOnlineTraversal(t *testing.T) {
	for _, streamed := range []bool{false, true} {
		t.Run(map[bool]string{false: "final", true: "streamed"}[streamed], func(t *testing.T) {
			testBackgroundPaths(t, streamed)
		})
	}
}

func testBackgroundPaths(t *testing.T, streamed bool) {
	states := []pool.State{testV2State(1, 1, 2, 1000000, 1000000), testV2State(2, 2, 1, 1000000, 1000000)}
	var ring []domain.PoolKey
	for index := byte(0); index < 8; index++ {
		from, to := byte(10+index), byte(11+index)
		if index == 0 {
			from = 1
		}
		if index == 7 {
			to = 1
		}
		p := testV2State(20+index, from, to, 1000000, 1300000)
		states, ring = append(states, p), append(ring, p.Key)
	}
	g, err := graph.Build(states)
	if err != nil {
		t.Fatal(err)
	}
	indexed, err := graph.BuildTable(g, 6, 100)
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, indexed, 1000, 1, v2CurveQuoter())
	if err := engine.EnableBackgroundExploration(g, graph.DefaultExploreLimits(6)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	opts := rankedOptions(states[0].Token0, 100000)
	opts.StreamInitial = streamed
	w, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-w.Done() })
	// No price job has ever been submitted. Quiet pools must already have
	// supplemental routes when their first dirty update arrives.
	var cache rankCache
	deadline := time.Now().Add(3 * time.Second)
	var result graph.Exploration
	for time.Now().Before(deadline) {
		result, err = cache.explore(t.Context(), engine, []domain.PoolKey{ring[0]})
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Cycles) > 0 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if result.CatalogPaths == 0 || len(result.Cycles) != 2 || result.Steps != 0 || cache.explorer != nil {
		t.Fatal("online path traversal or missing background coverage", result)
	}
	for _, cycle := range result.Cycles {
		if cycle.Len != 8 || !slices.Contains(cycle.Pools, ring[0]) {
			t.Fatal("wrong dirty-path index", cycle)
		}
	}
	quiet, err := cache.explore(t.Context(), engine, []domain.PoolKey{states[0].Key})
	if err != nil || len(quiet.Cycles) != 0 {
		t.Fatal("unaffected supplemental paths were ranked", quiet, err)
	}
	view := newFakeView()
	for _, p := range states {
		view.add(p)
	}
	prepared := make(map[domain.PoolKey]int)
	job := SearchJob{Incremental: true, View: view, Dirty: ring[:1], GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1},
		PrepareExact: func(_ context.Context, current View, keys []domain.PoolKey) (View, []QuoteIssue, error) {
			for _, key := range keys {
				prepared[key]++
			}
			return current, nil, nil
		},
	}
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	published, initialCount, optimized := 0, 0, 0
	for {
		event := nextSearch(t, w)
		if event.Err != nil || !w.Rebase(&event) || !w.Accept(event) {
			t.Fatal("background route search", event.Err)
		}
		for _, quote := range event.Result.Opportunities {
			if !slices.Equal(quote.Cycle.Pools, ring) || quote.Net.Sign() <= 0 {
				t.Fatal("wrong exact background quote", quote)
			}
			published++
			if event.Stage == QuoteInitial {
				initialCount++
			} else if event.Stage == QuoteOptimized {
				optimized++
			}
		}
		releaseSearch(event.Job)
		if !event.Partial {
			if event.Result.Exploration.Steps != 0 || event.Result.Selected != 1 || optimized != 1 || (streamed && (initialCount != 1 || published != 2)) || (!streamed && published != 1) {
				t.Fatal("online traversal or incorrect global K", event.Result)
			}
			break
		}
	}
	for _, key := range ring {
		if prepared[key] != 1 {
			t.Fatal("supplemental route fetched an exact dependency repeatedly", key, prepared[key])
		}
	}
	old := engine.exploration.catalog.Load()
	replacement := mustEngine(t, indexed, 1000, 1, v2CurveQuoter())
	if err := replacement.EnableBackgroundExploration(g, graph.DefaultExploreLimits(6)); err != nil {
		t.Fatal(err)
	}
	if err := w.ReplaceEngine(replacement, 2); err != nil {
		t.Fatal(err)
	}
	initial, err := cache.explore(t.Context(), replacement, ring[:1])
	if err != nil || len(initial.Cycles) != 0 || old.table.Len() != 2 {
		t.Fatal("catalog leaked across graph replacement", initial, err)
	}
}
