package arb

import (
	"math/big"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestEstimateMemoCollisionsAndConcurrentOwnership(t *testing.T) {
	var memo estimateMemo
	memo.reset()
	first := estimateKey{1, 0}
	other := estimateKey{2, 1}
	for estimateSlot(other) != estimateSlot(first) {
		other.path++
	}
	var workers sync.WaitGroup
	for i, key := range []estimateKey{first, other} {
		workers.Go(func() {
			input, net := big.NewInt(int64(i+1)), big.NewInt(int64(i+101))
			gotInput, gotNet := new(big.Int), new(big.Int)
			for range 1000 {
				memo.store(key, 1, 2, 1, input, net)
				if memo.load(key, 1, 2, 1, gotInput, gotNet) && (gotInput.Cmp(input) != 0 || gotNet.Cmp(net) != 0) {
					t.Error("collision returned another path's numbers")
					return
				}
				gotInput.SetInt64(-1)
				gotNet.SetInt64(-1)
			}
		})
	}
	workers.Wait()
	memo.store(first, 1, 2, 4, big.NewInt(3), big.NewInt(4))
	input, net := new(big.Int), new(big.Int)
	if !memo.load(first, 1, 2, 4, input, net) || input.Int64() != 3 || net.Int64() != 4 {
		t.Fatal("completed estimate not reused", input, net)
	}
	if memo.load(first, 2, 2, 4, input, net) || memo.load(first, 1, 3, 4, input, net) {
		t.Fatal("stale revisions reused")
	}
	memo.reset()
	if memo.load(first, 1, 2, 4, input, net) {
		t.Fatal("layout reset retained an estimate")
	}
}

func TestCompiledMemoKeepsUnselectedPathsAndRefreshesAfterEpochChange(t *testing.T) {
	states := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000), testV2State(3, 2, 1, 1000000, 2500000)}
	table := mustTable(t, 2, states...)
	engine := mustEngine(t, table, 1000, 1, identityQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView().add(states[0]).add(states[1]).add(states[2])
	dirty := []domain.PoolKey{states[0].Key}
	opts := rankedOptions(states[0].Token0, 100000)
	var cache rankCache
	cache.bind(engine.compiled, 1)
	selection, err := engine.rank(t.Context(), view, dirty, opts, &cache)
	if err != nil || len(selection.candidates) != 1 {
		t.Fatal(selection, err)
	}
	entries := 0
	for i := range cache.estimates.slots {
		if cache.estimates.slots[i].revision > 0 {
			entries++
		}
	}
	if entries <= len(selection.candidates) {
		t.Fatal("unselected full estimates were discarded", entries)
	}
	cache.bind(engine.compiled, 2)
	if !hasCachedEstimates(&cache) {
		t.Fatal("unchanged numerical estimates lost on epoch transition")
	}
	// Provider-owned integers can change on a branch/epoch transition. Rereading
	// them must invalidate both full estimates and directed-prefix coefficients.
	states[0].Reserve1.SetInt64(100000)
	cached, err := engine.rank(t.Context(), view, dirty, opts, &cache)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRanking(t, cached, fresh)
	states[0].Reserve1.SetInt64(2000000)
	cached, err = engine.rank(t.Context(), view, dirty, opts, &cache)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err = engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRanking(t, cached, fresh)
}

func TestSharedPrefixCoefficientsFollowDirectionalFeeAndSuffixChanges(t *testing.T) {
	states := []pool.State{
		testV2State(1, 1, 2, 1000000, 1500000), testV2State(2, 2, 3, 1000000, 1500000), testV2State(3, 3, 4, 1000000, 1500000),
		testV2State(4, 4, 5, 1000000, 1500000), testV2State(5, 5, 1, 1000000, 1500000), testV2State(6, 4, 5, 1000000, 1700000),
	}
	table := mustTable(t, 5, states...)
	engine := mustEngine(t, table, 1000, 3, identityQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView()
	var dirty []domain.PoolKey
	for _, p := range states {
		view.add(p)
		dirty = append(dirty, p.Key)
	}
	opts := rankedOptions(states[0].Token0, 100000)
	opts.Bounds = func(token domain.Address) (InputBounds, bool) {
		return InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, token == states[0].Token0 || token == states[2].Token0
	}
	var fee uint32
	opts.SwapFee = func(p pool.State, token domain.Address) (uint32, error) {
		if p.Key == states[1].Key && token == p.Token0 {
			return fee, nil
		}
		return p.Fee, nil
	}
	var cache rankCache
	cache.bind(engine.compiled, 1)
	for stage := range 5 {
		if stage == 1 {
			states[5].Reserve1.SetInt64(1200000)
		}
		if stage == 2 {
			fee = 500000
		}
		if stage == 3 {
			states[1].Reserve1.SetInt64(1800000)
		}
		if stage == 4 {
			fee = 0
			cache.bind(engine.compiled, 2)
		}
		for _, workers := range []int{1, 4} {
			cached, err := engine.rankWithWorkers(t.Context(), view, dirty, opts, &cache, workers)
			if err != nil {
				t.Fatal(err)
			}
			generic := mustEngine(t, table, 1000, 3, identityQuoter())
			fresh, err := generic.Rank(t.Context(), view, dirty, opts)
			if err != nil {
				t.Fatal(err)
			}
			assertSameRanking(t, cached, fresh)
		}
	}
}
