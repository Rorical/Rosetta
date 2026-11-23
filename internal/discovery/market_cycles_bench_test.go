package discovery

import (
	"context"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

const benchmarkDepth = 6

type selectFunc func(ctx context.Context, pools []pool.State, activity map[domain.PoolKey]Activity, policy SelectionPolicy, maxPools, depth int, pinned []domain.PoolKey) (Selection, error)

// benchmarkDiscoveryRuns times one discovery run: a candidate pass over the
// universe and a recheck over the catalog plus a third of the candidates.
// Runs alternate between two universes that differ by a few long-tail markets
// entering and leaving, as the candidate queue rotates.
func benchmarkDiscoveryRuns(b *testing.B, run selectFunc) {
	m := newSyntheticMarket(1, 200)
	pinned := map[domain.PoolKey]bool{}
	for _, key := range m.pinned {
		pinned[key] = true
	}
	first := slices.Clone(m.pools)
	for range 6 {
		m.addTail()
	}
	i := 0
	second := slices.DeleteFunc(slices.Clone(m.pools), func(p pool.State) bool {
		i++
		return !pinned[p.Key] && i%25 == 0
	})
	recheck := func(universe []pool.State) []pool.State {
		j := 0
		return slices.DeleteFunc(slices.Clone(universe), func(p pool.State) bool {
			j++
			return !pinned[p.Key] && j%3 != 0
		})
	}
	universes := [][]pool.State{first, second}
	rechecks := [][]pool.State{recheck(first), recheck(second)}
	b.Logf("pools=%d/%d pinned=%d", len(first), len(second), len(m.pinned))
	policy := m.policy()
	b.ReportAllocs()
	round := 0
	for b.Loop() {
		for _, pools := range [][]pool.State{universes[round%2], rechecks[round%2]} {
			if _, err := run(b.Context(), pools, nil, policy, 1024, benchmarkDepth, m.pinned); err != nil {
				b.Fatal(err)
			}
		}
		round++
	}
}

func BenchmarkSelectMarketsFull(b *testing.B) {
	m := newSyntheticMarket(1, 200)
	b.Logf("pools=%d pinned=%d", len(m.pools), len(m.pinned))
	policy := m.policy()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := SelectMarketsWithPinned(b.Context(), m.pools, nil, policy, 1024, benchmarkDepth, m.pinned); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSelectMarketsCachedUnchanged(b *testing.B) {
	m := newSyntheticMarket(1, 200)
	policy := m.policy()
	var selector MarketSelector
	b.ReportAllocs()
	for b.Loop() {
		if _, err := selector.Select(b.Context(), m.pools, nil, policy, 1024, benchmarkDepth, m.pinned); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDiscoveryRunFresh(b *testing.B) {
	benchmarkDiscoveryRuns(b, SelectMarketsWithPinned)
}

func BenchmarkDiscoveryRunCached(b *testing.B) {
	var selector MarketSelector
	benchmarkDiscoveryRuns(b, selector.Select)
}
