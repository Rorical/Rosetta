package arb

import (
	"context"
	"math/big"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestWorkerFindsUnindexedEightHopCycleWithSharedPreparationAndGlobalK(t *testing.T) {
	states := []pool.State{testV2State(1, 1, 2, 1_000_000, 1_020_000), testV2State(2, 2, 1, 1_000_000, 1_020_000)}
	var ring []domain.PoolKey
	for i := byte(0); i < 8; i++ {
		from, to := byte(10+i), byte(11+i)
		if i == 0 {
			from = 1
		}
		if i == 7 {
			to = 1
		}
		p := testV2State(20+i, from, to, 1_000_000, 1_300_000)
		states = append(states, p)
		ring = append(ring, p.Key)
	}
	g, err := graph.Build(states)
	if err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTable(g, 6, 100)
	if err != nil {
		t.Fatal(err)
	}
	table, err = graph.PackTable(t.Context(), table, states)
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, table, 1000, 1, v2CurveQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	if err := engine.EnableExploration(g, graph.DefaultExploreLimits(6)); err != nil {
		t.Fatal(err)
	}
	view := newFakeView()
	for _, p := range states {
		view.add(p)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker, err := NewWorker(ctx, engine, rankedOptions(states[0].Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	for _, source := range []domain.StateSource{domain.SourceStandard, domain.SourceFast} {
		var prepared, exact []domain.PoolKey
		job := SearchJob{View: view, Dirty: []domain.PoolKey{ring[0], states[0].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: source, Epoch: 1},
			Prepare: func(_ context.Context, view View, keys []domain.PoolKey) (View, []QuoteIssue, error) {
				prepared = slices.Clone(keys)
				return view, nil, nil
			},
			PrepareExact: func(_ context.Context, view View, keys []domain.PoolKey) (View, []QuoteIssue, error) {
				exact = slices.Clone(keys)
				return view, nil, nil
			},
		}
		if err := worker.Submit(job); err != nil {
			t.Fatal(err)
		}
		event := workerResult(t, worker, job.Version)
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		if event.Result.Exploration.Discovered == 0 || event.Result.Exploration.Steps > 4096 || event.Result.Selected != 1 || len(event.Result.Opportunities) != 1 {
			t.Fatalf("bounded exploration or global K lost: %+v", event.Result)
		}
		if !slices.Equal(event.Result.Opportunities[0].Cycle.Pools, ring) {
			t.Fatal("did not select the profitable eight-hop path")
		}
		for _, key := range ring {
			if !slices.Contains(prepared, key) || !slices.Contains(exact, key) {
				t.Fatal("unindexed dependency omitted from preparation")
			}
		}
		if !worker.Accept(event) {
			t.Fatal("current explored result rejected")
		}
	}
}

func TestExplorationCacheSurvivesStateEpochAndRetiresAtEngineReplacement(t *testing.T) {
	states := []pool.State{testV2State(1, 1, 2, 1000, 2000), testV2State(2, 2, 1, 1000, 2000)}
	g, err := graph.Build(states)
	if err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTableFromCycles(nil)
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, table, 1, 1, identityQuoter())
	if err := engine.EnableExploration(g, graph.DefaultExploreLimits(2)); err != nil {
		t.Fatal(err)
	}
	var cache rankCache
	first, err := cache.explore(t.Context(), engine, []domain.PoolKey{states[0].Key})
	if err != nil || first.Added == 0 {
		t.Fatal(first, err)
	}
	original := cache.explorer
	_, err = cache.explore(t.Context(), engine, []domain.PoolKey{states[0].Key})
	if err != nil || cache.explorer != original {
		t.Fatal("same lane lost cursors")
	}
	cache.epoch = 2
	_, err = cache.explore(t.Context(), engine, []domain.PoolKey{states[0].Key})
	if err != nil || cache.explorer != original {
		t.Fatal("state epoch discarded immutable path traversal")
	}
	original = cache.explorer
	_, err = cache.explore(t.Context(), engine.WithGas(GasConfig{Price: big.NewInt(1)}), []domain.PoolKey{states[0].Key})
	if err != nil || cache.explorer == original {
		t.Fatal("engine replacement reused traversal")
	}
}

// Preparation keys must equal the sorted, compacted concatenation they replace,
// including pools whose keys differ only after the address.
func TestCyclePoolsMatchesSortedUnion(t *testing.T) {
	key := func(address byte, kind domain.PoolKind, id byte) domain.PoolKey {
		return domain.PoolKey{Address: domain.Address{address}, Kind: kind, PoolID: domain.Hash{id}}
	}
	dirty := []domain.PoolKey{key(9, 4, 1), key(1, 1, 0), key(9, 4, 1)}
	cycles := []graph.Cycle{
		{Pools: []domain.PoolKey{key(3, 1, 0), key(9, 4, 2), key(1, 1, 0)}},
		{Pools: []domain.PoolKey{key(9, 4, 1), key(3, 2, 0), key(2, 1, 0)}},
	}
	want := slices.Clone(dirty)
	for _, cycle := range cycles {
		want = append(want, cycle.Pools...)
	}
	slices.SortFunc(want, domain.ComparePoolKeys)
	want = slices.Compact(want)
	var cache rankCache
	for range 2 { // The reused set must not leak keys into the next frame.
		if got := cache.cyclePools(dirty, cycles); !slices.Equal(got, want) {
			t.Fatal("preparation keys differ", got, want)
		}
	}
	if got := cache.cyclePools(nil, cycles[:1]); len(got) != 3 {
		t.Fatal("stale preparation keys", got)
	}
}
