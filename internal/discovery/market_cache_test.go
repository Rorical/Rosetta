package discovery

import (
	"math/big"
	"reflect"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// cachedRun selects through the cache and fails unless the result equals a
// fresh enumeration. It returns how many candidates reused an earlier entry.
func cachedRun(t *testing.T, selector *MarketSelector, pools []pool.State, pinned []domain.PoolKey, policy SelectionPolicy, depth int) (Selection, int) {
	t.Helper()
	before := map[domain.PoolKey]*anchorEntry{}
	for key, entry := range selector.cycles.entries {
		before[key] = entry
	}
	got, err := selector.Select(t.Context(), pools, nil, policy, 300, depth, pinned)
	if err != nil {
		t.Fatal(err)
	}
	want, err := SelectMarketsWithPinned(t.Context(), pools, nil, policy, 300, depth, pinned)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cached selection differs: %d keys (truncated %v, capped %d), fresh %d keys (truncated %v, capped %d)",
			len(got.Keys), got.Truncated, got.CappedCandidates, len(want.Keys), want.Truncated, want.CappedCandidates)
	}
	reused := 0
	for key, entry := range selector.cycles.entries {
		if before[key] == entry && entry.read == selector.cycles.round {
			reused++
		}
	}
	return got, reused
}

func dropPools(pools []pool.State, drop func(pool.State) bool) []pool.State {
	return slices.DeleteFunc(slices.Clone(pools), drop)
}

func TestMarketSelectorMatchesFreshSelectionAcrossRuns(t *testing.T) {
	m := newSyntheticMarket(7, 120)
	policy := m.policy()
	pools, pinned := slices.Clone(m.pools), slices.Clone(m.pinned)
	isPinned := func(key domain.PoolKey) bool { return slices.Contains(pinned, key) }
	var selector MarketSelector
	steps := []struct {
		name      string
		depth     int
		change    func()
		wantReuse bool
	}{
		{name: "initial", depth: 6, change: func() {}},
		{name: "unchanged", depth: 6, change: func() {}, wantReuse: true},
		{name: "recheck drops half the candidates", depth: 6, wantReuse: true, change: func() {
			i := 0
			pools = dropPools(pools, func(p pool.State) bool { i++; return !isPinned(p.Key) && i%2 == 0 })
		}},
		{name: "next run restores candidates", depth: 6, change: func() { pools = slices.Clone(m.pools) }},
		{name: "new long-tail markets", depth: 6, change: func() {
			for range 12 {
				m.addTail()
			}
			pools = slices.Clone(m.pools)
		}},
		{name: "parallel market on a candidate pair", depth: 6, change: func() {
			for _, p := range pools {
				if !isPinned(p.Key) {
					m.addPool(p.Token1, p.Token0)
					break
				}
			}
			pools = slices.Clone(m.pools)
		}},
		{name: "drained pools leave eligibility", depth: 6, change: func() {
			for i := 0; i < len(pools); i += 9 {
				pools[i].Reserve0, pools[i].Reserve1 = big.NewInt(10), big.NewInt(10)
			}
		}},
		{name: "drained pools recover", depth: 6, change: func() { pools = slices.Clone(m.pools) }},
		{name: "candidates promoted to the catalog", depth: 6, wantReuse: true, change: func() {
			for i, p := range pools {
				if !isPinned(p.Key) && i%3 == 0 {
					pinned = append(pinned, p.Key)
				}
			}
		}},
		{name: "same key with other tokens", depth: 6, change: func() {
			for i, p := range pools {
				if !isPinned(p.Key) {
					pools[i].Token1 = m.hubs[3]
					if p.Token0 == m.hubs[3] {
						pools[i].Token1 = m.hubs[2]
					}
					break
				}
			}
		}},
		{name: "shallower depth", depth: 4, change: func() {}},
		{name: "shallower depth unchanged", depth: 4, change: func() {}, wantReuse: true},
		{name: "two-pool depth", depth: 2, change: func() {}},
		{name: "depth restored", depth: 6, change: func() {}},
		{name: "rotation", depth: 6, change: func() {
			i := 0
			pools = dropPools(pools, func(p pool.State) bool { i++; return !isPinned(p.Key) && i%4 == 1 })
			for range 6 {
				m.addTail()
			}
			pools = append(pools, m.pools[len(m.pools)-8:]...)
			slices.SortFunc(pools, func(a, b pool.State) int { return domain.ComparePoolKeys(a.Key, b.Key) })
			pools = slices.CompactFunc(pools, func(a, b pool.State) bool { return a.Key == b.Key })
		}},
		{name: "rotation unchanged", depth: 6, change: func() {}, wantReuse: true},
	}
	capped := false
	for _, step := range steps {
		step.change()
		got, reused := cachedRun(t, &selector, pools, pinned, policy, step.depth)
		capped = capped || got.CappedCandidates > 0
		if step.wantReuse && reused == 0 {
			t.Fatalf("%s: no cached candidate was reused", step.name)
		}
	}
	if !capped {
		t.Fatal("fixture never filled a candidate budget")
	}
}

// fanMarket has anchor a-b plus spokes a-c-b, so the anchor's deep walk finds
// two directed three-pool cycles per spoke and no two-pool cycle.
func fanMarket(spokes int) ([]pool.State, domain.PoolKey) {
	a, b := domain.Address{1}, domain.Address{2}
	pools := []pool.State{marketState(1, a, b, 1<<40, 1<<40)}
	for i := range spokes {
		c := domain.Address{3, byte(i)}
		pools = append(pools, marketState(byte(10+2*i), a, c, 1<<40, 1<<40), marketState(byte(11+2*i), c, b, 1<<40, 1<<40))
	}
	return pools, pools[0].Key
}

func TestMarketSelectorTracksDeepBudgetTruncation(t *testing.T) {
	policy := SelectionPolicy{Targets: []SelectionTarget{{Token: domain.Address{1}.Hex(), Amount: "1000"}}, MaxTokens: 256, MaxSlippageBPS: 100, ActivityWindow: 100, ProbeLimit: 32}
	cases := []struct {
		name       string
		spokes     int
		drop       int // spoke whose c-b pool is removed in the second run
		wantCapped [2]int
		wantReuse  bool
	}{
		// 50 cycles fill the 48 budget; losing a spoke leaves exactly 48.
		{name: "removal clears truncation", spokes: 25, drop: 0, wantCapped: [2]int{1, 0}},
		// A spoke past the stored prefix and its witness cannot change them.
		{name: "removal beyond witness", spokes: 60, drop: 59, wantCapped: [2]int{1, 1}, wantReuse: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pools, anchor := fanMarket(tc.spokes)
			var pinned []domain.PoolKey
			for _, p := range pools[1:] {
				pinned = append(pinned, p.Key)
			}
			var selector MarketSelector
			first, _ := cachedRun(t, &selector, pools, pinned, policy, 3)
			gone := pools[2+2*tc.drop].Key
			if len(selector.cycles.entries[anchor].deep) != candidateDeepCycles+1 {
				t.Fatal("deep walk did not keep its truncation witness")
			}
			pools = dropPools(pools, func(p pool.State) bool { return p.Key == gone })
			pinned = slices.DeleteFunc(pinned, func(key domain.PoolKey) bool { return key == gone })
			second, reused := cachedRun(t, &selector, pools, pinned, policy, 3)
			if [2]int{first.CappedCandidates, second.CappedCandidates} != tc.wantCapped {
				t.Fatalf("capped candidates %d then %d, want %v", first.CappedCandidates, second.CappedCandidates, tc.wantCapped)
			}
			if (reused > 0) != tc.wantReuse {
				t.Fatalf("reused %d entries, want reuse %v", reused, tc.wantReuse)
			}
		})
	}
}

func TestMarketSelectorResetForgetsEntries(t *testing.T) {
	m := newSyntheticMarket(3, 40)
	var selector MarketSelector
	cachedRun(t, &selector, m.pools, m.pinned, m.policy(), 6)
	selector.Reset()
	if _, reused := cachedRun(t, &selector, m.pools, m.pinned, m.policy(), 6); reused != 0 {
		t.Fatal("reset selector reused entries", reused)
	}
}
