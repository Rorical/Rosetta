package graph

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// denseExplorer builds a graph with many parallel pools between few tokens so
// exploration finds far more cycles than a small cache holds.
func denseExplorer(tb testing.TB, cachePaths int) (*Explorer, []domain.PoolKey) {
	tb.Helper()
	var states []pool.State
	address := byte(1)
	for a := byte(1); a <= 5; a++ {
		for b := a + 1; b <= 5; b++ {
			for parallel := 0; parallel < 3; parallel++ {
				states = append(states, pool.State{Key: domain.PoolKey{Address: domain.Address{address, a, b}, Kind: domain.PoolKindV2}, Token0: domain.Address{a}, Token1: domain.Address{b}})
				address++
			}
		}
	}
	g, err := Build(states)
	if err != nil {
		tb.Fatal(err)
	}
	indexed, err := PackTable(tb.Context(), &Table{}, states)
	if err != nil {
		tb.Fatal(err)
	}
	explorer, err := NewExplorer(g, indexed, ExploreLimits{MaxDepth: 4, Steps: 256, NewPaths: 8, CachePaths: cachePaths})
	if err != nil {
		tb.Fatal(err)
	}
	keys := make([]domain.PoolKey, len(states))
	for i, p := range states {
		keys[i] = p.Key
	}
	return explorer, keys
}

// Exploration returns the cached paths touching dirty pools in identity order,
// independent of insertion and eviction history.
func TestExplorerReturnsCachedPathsInIdentityOrderAcrossEvictions(t *testing.T) {
	explorer, keys := denseExplorer(t, 24)
	random := rand.New(rand.NewPCG(1, 2))
	evicted := false
	for turn := 0; turn < 400; turn++ {
		dirty := keys
		if turn%3 != 0 {
			dirty = nil
			for _, key := range keys {
				if random.IntN(4) == 0 {
					dirty = append(dirty, key)
				}
			}
		}
		result, err := explorer.Explore(t.Context(), dirty)
		if err != nil {
			t.Fatal(err)
		}
		evicted = evicted || result.CacheTruncated
		var want []Cycle
		for _, entry := range explorer.cache {
			if slices.ContainsFunc(entry.cycle.Pools, func(key domain.PoolKey) bool { return slices.Contains(dirty, key) }) {
				want = append(want, entry.cycle)
			}
		}
		slices.SortFunc(want, compareCycleIdentity)
		if !slices.EqualFunc(result.Cycles, want, func(a, b Cycle) bool { return compareCycleIdentity(a, b) == 0 }) {
			t.Fatalf("turn %d: %d paths, want %d in identity order", turn, len(result.Cycles), len(want))
		}
	}
	if !evicted {
		t.Fatal("fixture never evicted a cached path")
	}
}

// The background maintainer explores every pool each tick once its cache is
// full; this is that steady state.
func BenchmarkExploreFullCacheAllDirty(b *testing.B) {
	explorer, keys := denseExplorer(b, 8192)
	for explorer.Explore(b.Context(), keys); !explorer.evicted && len(explorer.cache) < 2048; {
		if _, err := explorer.Explore(b.Context(), keys); err != nil {
			b.Fatal(err)
		}
		if explorer.jobs[explorer.next].done && len(explorer.cache) > 0 {
			break
		}
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := explorer.Explore(b.Context(), keys); err != nil {
			b.Fatal(err)
		}
	}
}
