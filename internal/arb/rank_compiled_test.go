package arb

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func assertSameRanking(t *testing.T, a, b RankedSelection) {
	t.Helper()
	// Cache identities are not part of the public ranking result.
	for _, selection := range []*RankedSelection{&a, &b} {
		selection.candidates = append([]rankedCycle(nil), selection.candidates...)
		for i := range selection.candidates {
			selection.candidates[i].path = 0
			selection.candidates[i].rotation = 0
			selection.candidates[i].revision = 0
		}
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("compiled ranking differs: considered=%d/%d candidates=%d/%d issues=%v/%v", a.considered, b.considered, len(a.candidates), len(b.candidates), a.issues, b.issues)
	}
}

func TestCompiledRankingAndCacheFollowActualPoolAndFundingChanges(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	third := testV2State(3, 2, 1, 1_000_000, 2_500_000)
	states := []pool.State{first, second, third}
	table := mustTable(t, 2, states...)
	generic := mustEngine(t, table, 1000, 2, identityQuoter())
	compiled := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := compiled.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	if compiled.WithGas(GasConfig{}).compiled != compiled.compiled {
		t.Fatal("gas configuration lost compiled metadata")
	}
	view := newFakeView().add(first).add(second).add(third)
	dirty := []domain.PoolKey{first.Key, second.Key, third.Key}
	opts := rankedOptions(first.Token0, 100000)
	var cache rankCache
	cache.bind(compiled.compiled, 1)
	check := func() RankedSelection {
		t.Helper()
		want, err := generic.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := compiled.rank(t.Context(), view, dirty, opts, &cache)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, got)
		uncached, err := compiled.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, uncached)
		return got
	}
	retained := check()
	check() // Warm positive estimates.
	if !hasCachedEstimates(&cache) {
		t.Fatal("positive estimates were not cached")
	}
	first.Reserve1.SetInt64(100_000) // Mutate a provider-owned integer in place.
	check()
	first.Reserve1.SetInt64(2_000_000)
	check()
	changed := first
	changed.Fee = 900_000
	view.states[first.Key] = changed
	check()
	view.states[first.Key] = first
	opts = rankedOptions(first.Token0, 100)
	check()
	if err := compiled.SetK(1); err != nil {
		t.Fatal(err)
	}
	if err := generic.SetK(1); err != nil {
		t.Fatal(err)
	}
	check()
	delete(view.states, second.Key)
	check()
	view.add(second)
	cache.bind(compiled.compiled, 2)
	// A lineage change rereads numerical inputs; it need not discard estimates
	// whose actual dependencies are unchanged.

	check()
	// Owned selections survive cache updates and input mutation.
	if len(retained.candidates) == 0 || retained.candidates[0].bounds.Max.Int64() != 100000 {
		t.Fatal("cache mutated a delivered selection")
	}
}

func TestCompiledRankingPreservesMissingDependenciesAndCatalogFallback(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	broken := testV3State(3, 2, 1, 3000, 60, 0)
	states := []pool.State{first, second, broken}
	table := mustTable(t, 2, states...)
	generic := mustEngine(t, table, 1000, 2, identityQuoter())
	compiled := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := compiled.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView().add(first).add(second).add(broken)
	opts := rankedOptions(first.Token0, 100000)
	feeFailure := errors.New("directional fee unavailable")
	opts.SwapFee = func(p pool.State, token domain.Address) (uint32, error) {
		if p.Key == second.Key && token == p.Token0 {
			return 0, feeFailure
		}
		return p.Fee, nil
	}
	dirty := []domain.PoolKey{first.Key, second.Key, broken.Key}
	for stage := range 3 {
		if stage == 1 {
			changed := first
			changed.Token0 = testAddress(9)
			view.states[first.Key] = changed
		}
		if stage == 2 {
			view.add(first)
			delete(view.states, broken.Key)
		}
		want, err := generic.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := compiled.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, got)
	}
}

func TestCompiledTopologyHandlesLongRoutesAndCanceledBuilds(t *testing.T) {
	for _, depth := range []int{6, 16} {
		var states []pool.State
		var keys []domain.PoolKey
		for i := range depth {
			p := testV2State(byte(i+1), byte(i+1), byte((i+1)%depth+1), 1_000_000, 1_100_000)
			states = append(states, p)
			keys = append(keys, p.Key)
		}
		table, err := graph.BuildTableFromCycles([]graph.Cycle{{Start: testAddress(1), Pools: keys, Len: depth}})
		if err != nil {
			t.Fatal(err)
		}
		generic := mustEngine(t, table, 1000, 1, identityQuoter())
		compiled := mustEngine(t, table, 1000, 1, identityQuoter())
		canceled, cancel := context.WithCancel(t.Context())
		cancel()
		if err := compiled.PreparePaths(canceled, states); err != context.Canceled {
			t.Fatal(err)
		}
		if compiled.compiled != nil {
			t.Fatal("canceled build published a partial topology")
		}
		if err := compiled.PreparePaths(t.Context(), states); err != nil {
			t.Fatal(err)
		}
		view := newFakeView()
		for _, p := range states {
			view.add(p)
		}
		bounds := InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}
		opts := RankedOptions{Bounds: func(domain.Address) (InputBounds, bool) { return bounds, true }, SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil }}
		a, err := generic.Rank(t.Context(), view, keys, opts)
		if err != nil {
			t.Fatal(err)
		}
		b, err := compiled.Rank(t.Context(), view, keys, opts)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, a, b)
	}
}

func TestCompiledV4SiblingsKeepPoolIdentityAndDirectionalFees(t *testing.T) {
	first := testV3State(1, 1, 2, 3000, 60, 1_000_000)
	second := testV3State(1, 1, 2, 3000, 60, 1_000_000)
	first.Key.Kind, second.Key.Kind = domain.PoolKindV4, domain.PoolKindV4
	first.Key.PoolID[31], second.Key.PoolID[31] = 1, 2
	first.SqrtPriceX96.Mul(first.SqrtPriceX96, big.NewInt(12)).Div(first.SqrtPriceX96, big.NewInt(10))
	states := []pool.State{first, second}
	table := mustTable(t, 2, states...)
	generic := mustEngine(t, table, 1000, 2, identityQuoter())
	compiled := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := compiled.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView().add(first).add(second)
	opts := rankedOptions(first.Token0, 100000)
	opts.SwapFee = func(p pool.State, token domain.Address) (uint32, error) {
		if token == p.Token0 {
			return 1000, nil
		}
		return 3000, nil
	}
	dirty := []domain.PoolKey{first.Key, second.Key}
	want, err := generic.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := compiled.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.candidates) == 0 {
		t.Fatal("V4 fixture has no profitable direction")
	}
	assertSameRanking(t, want, got)
	// Shards reduce independently but retain identical path and tie order.
	snapshot := compiledSnapshots.Get().(*compiledSnapshot)
	defer snapshot.release()
	var buffer graph.CandidateBuffer
	indices := buffer.Select(table, dirty, table.Len())
	compatible, err := snapshot.prepare(t.Context(), compiled.compiled, indices, view, opts, 1)
	if err != nil || !compatible {
		t.Fatal(compatible, err)
	}
	var cache rankCache
	cache.bind(compiled.compiled, 1)
	if err := cache.update(t.Context(), snapshot, 1); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		partitioned, err := rankInShards(indices, compiled.K(), 4, false, func(part []int, limit int) (RankedSelection, error) {
			return compiled.rankCompiled(t.Context(), snapshot, part, limit, &cache, false)
		})
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, partitioned)
	}
}

func TestCanceledCompiledPassRetainsOnlyCompletedNegativeDecisions(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 1_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 1_000_000)
	states := []pool.State{first, second}
	table := mustTable(t, 2, states...)
	engine := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView().add(first).add(second)
	dirty := []domain.PoolKey{first.Key}
	opts := rankedOptions(first.Token0, 100000)
	var buffer graph.CandidateBuffer
	indices := buffer.Select(table, dirty, table.Len())
	snapshot := compiledSnapshots.Get().(*compiledSnapshot)
	defer snapshot.release()
	compatible, err := snapshot.prepare(t.Context(), engine.compiled, indices, view, opts, 1)
	if err != nil || !compatible {
		t.Fatal(compatible, err)
	}
	var cache rankCache
	cache.bind(engine.compiled, 1)
	if err := cache.update(t.Context(), snapshot, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := engine.rankCompiled(t.Context(), snapshot, indices[:1], engine.K(), &cache, false); err != nil {
		t.Fatal(err)
	}
	if cache.negative[indices[0]] == 0 || cache.negative[indices[1]] != 0 {
		t.Fatal("unfinished work was cached")
	}
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := engine.rankCompiled(canceled, snapshot, indices[1:], engine.K(), &cache, false); err != context.Canceled {
		t.Fatal(err)
	}
	got, err := engine.rank(t.Context(), view, dirty, opts, &cache)
	if err != nil {
		t.Fatal(err)
	}
	want, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRanking(t, want, got)
	// A changed pool must invalidate a cached zero-profit decision.
	first.Reserve1.SetInt64(2_000_000)
	got, err = engine.rank(t.Context(), view, dirty, opts, &cache)
	if err != nil {
		t.Fatal(err)
	}
	want, err = engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRanking(t, want, got)
	if len(got.candidates) == 0 {
		t.Fatal("cached rejection hid the new profitable path")
	}
	replacement := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := replacement.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	cache.bind(replacement.compiled, 1)
	if hasCachedEstimates(&cache) {
		t.Fatal("topology replacement retained old estimates")
	}
}

func TestCompiledCacheDistinguishesNilAndEmptyFeeErrors(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	states := []pool.State{first, second}
	table := mustTable(t, 2, states...)
	engine := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView().add(first).add(second)
	dirty := []domain.PoolKey{first.Key}
	opts := rankedOptions(first.Token0, 100000)
	var failure error
	opts.SwapFee = func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, failure }
	var cache rankCache
	cache.bind(engine.compiled, 1)
	if _, err := engine.rank(t.Context(), view, dirty, opts, &cache); err != nil {
		t.Fatal(err)
	}
	failure = errors.New("")
	cached, err := engine.rank(t.Context(), view, dirty, opts, &cache)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	assertSameRanking(t, fresh, cached)
	if len(cached.candidates) != 0 || len(cached.issues) == 0 {
		t.Fatal("cached estimate hid a newly failed fee read")
	}
}

func hasCachedEstimates(cache *rankCache) bool {
	for i := range cache.estimates.slots {
		if cache.estimates.slots[i].revision != 0 {
			return true
		}
	}
	return false
}

func TestRankingFloorPrunesRoutesBelowTheCheapestTransaction(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	third := testV2State(3, 2, 1, 1_000_000, 2_500_000)
	states := []pool.State{first, second, third}
	table := mustTable(t, 2, states...)
	generic := mustEngine(t, table, 1000, 2, identityQuoter())
	compiled := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := compiled.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	view := newFakeView().add(first).add(second).add(third)
	dirty := []domain.PoolKey{first.Key, second.Key, third.Key}
	withFloor := func(floor *big.Int) RankedOptions {
		opts := rankedOptions(first.Token0, 100000)
		bounds := opts.Bounds
		opts.Bounds = func(token domain.Address) (InputBounds, bool) {
			b, ok := bounds(token)
			b.Floor = floor
			return b, ok
		}
		return opts
	}
	rank := func(opts RankedOptions) RankedSelection {
		t.Helper()
		want, err := generic.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := compiled.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, got)
		return got
	}
	open := rank(withFloor(nil))
	if len(open.candidates) == 0 {
		t.Fatal("fixture has no profitable route")
	}
	best := new(big.Int)
	for _, c := range open.candidates {
		if c.net.Cmp(best) > 0 {
			best.Set(c.net)
		}
	}
	if kept := rank(withFloor(best)); len(kept.candidates) == 0 {
		t.Fatal("route at the floor was pruned")
	}
	above := new(big.Int).Add(best, big.NewInt(1))
	if pruned := rank(withFloor(above)); len(pruned.candidates) != 0 {
		t.Fatal("routes below the floor reached exact quoting", len(pruned.candidates))
	}
	quote := Opportunity{Cycle: graph.Cycle{Start: first.Token0}, Net: best}
	if withFloor(above).publishable(quote) || !withFloor(best).publishable(quote) {
		t.Fatal("publication floor disagrees with ranking floor")
	}
}
