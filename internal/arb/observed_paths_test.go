package arb

import (
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestObservedPathsBypassTruncatedTableWithoutLosingDirtyIsolation(t *testing.T) {
	pools := []pool.State{testV2State(1, 1, 2, 1000, 2000), testV2State(2, 2, 1, 1000, 2000), testV2State(3, 2, 3, 1000, 2000), testV2State(4, 3, 1, 1000, 2000)}
	g, err := graph.Build(pools)
	if err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTableFromCycles([]graph.Cycle{{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}})
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, table, 1, 2, v2CurveQuoter())
	if err := engine.EnableBackgroundExploration(g, graph.DefaultExploreLimits(3)); err != nil {
		t.Fatal(err)
	}
	cycle := graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[2].Key, pools[3].Key}, Len: 3}
	engine.RememberCycles([]graph.Cycle{cycle, cycle, {Start: domain.Address{99}, Pools: cycle.Pools, Len: 3}})
	if len(engine.ObservedCycles()) != 1 {
		t.Fatal("duplicate or disconnected path retained")
	}
	cycle.Pools[0] = pools[1].Key // caller mutation must not change a published catalog
	var cache rankCache
	result, err := cache.catalogPaths(t.Context(), engine.exploration, []domain.PoolKey{pools[2].Key})
	if err != nil || len(result.Cycles) != 1 || result.Cycles[0].Pools[0] != pools[0].Key || result.Steps != 0 {
		t.Fatal(result, err)
	}
	snapshot := result.Cycles[0]
	quiet, err := cache.catalogPaths(t.Context(), engine.exploration, []domain.PoolKey{pools[1].Key})
	if err != nil || len(quiet.Cycles) != 0 {
		t.Fatal("unaffected observed cycle was scanned")
	}
	// The general traversal can publish the same route; online work stays unique.
	base, _ := graph.BuildTableFromCycles([]graph.Cycle{snapshot})
	engine.exploration.catalog.Store(&pathCatalog{table: base, cycles: []graph.Cycle{snapshot}})
	result, err = cache.catalogPaths(t.Context(), engine.exploration, []domain.PoolKey{pools[0].Key})
	if err != nil || len(result.Cycles) != 1 || !slices.Equal(result.Cycles[0].Pools, snapshot.Pools) {
		t.Fatal("duplicate supplemental path", err)
	}
}

// An hour of OP receipts holds about 820 distinct routes, and a route rivals
// reuse can have up to about 600 others between repeats. A route seen again is
// kept over routes seen once before it.
func TestObservedPathsEvictTheLeastRecentlySeen(t *testing.T) {
	var pools []pool.State
	for i := range 100 {
		pools = append(pools, testV2State(byte(i+1), 1, 2, 1000, 2000))
	}
	g, err := graph.Build(pools)
	if err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTableFromCycles([]graph.Cycle{{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}})
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, table, 1, 2, v2CurveQuoter())
	if err := engine.EnableBackgroundExploration(g, graph.DefaultExploreLimits(3)); err != nil {
		t.Fatal(err)
	}
	var cycles []graph.Cycle
	for i := range pools {
		for j := range pools {
			if i != j && len(cycles) < observedPathLimit+1 {
				cycles = append(cycles, graph.Cycle{Start: pools[i].Token0, Pools: []domain.PoolKey{pools[i].Key, pools[j].Key}, Len: 2})
			}
		}
	}
	reused, second := cycles[0], cycles[1]
	engine.RememberCycles(cycles[:observedPathLimit])
	engine.RememberCycles([]graph.Cycle{reused})
	engine.RememberCycles(cycles[observedPathLimit:])
	kept := engine.ObservedCycles()
	has := func(c graph.Cycle) bool {
		return slices.ContainsFunc(kept, func(k graph.Cycle) bool { return slices.Equal(k.Pools, c.Pools) })
	}
	if len(kept) != observedPathLimit || !has(reused) || has(second) {
		t.Fatalf("kept %d, reused %v, oldest unseen %v", len(kept), has(reused), has(second))
	}
}
