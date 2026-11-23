package arb

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestShortEngineQuotesShortRoutesLikeTheFullEngine(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	third := testV2State(3, 2, 1, 1_000_000, 2_500_000)
	states := []pool.State{first, second, third}
	full := mustEngine(t, mustTable(t, 2, states...), 1000, 2, identityQuoter())
	view := newFakeView().add(first).add(second).add(third)
	opts := rankedOptions(first.Token0, 100000)

	short, err := full.ShortEngine(t.Context(), 2, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if short.table.Len() != full.table.Len() {
		t.Fatalf("two-hop table lost routes: %d of %d", short.table.Len(), full.table.Len())
	}
	// Routes through the changed pools rank identically in both engines.
	dirty := []domain.PoolKey{third.Key}
	want, err := full.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := short.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	if len(want.candidates) == 0 {
		t.Fatal("fixture has no profitable route through the changed pool")
	}
	assertSameRanking(t, want, got)
	for _, index := range short.table.Candidates(dirty, 1) {
		if !containsPool(short.table.Cycle(index).Pools, third.Key) {
			t.Fatal("pool index returned a route that does not touch the changed pool")
		}
	}

	none, err := full.ShortEngine(t.Context(), 1, 2, 0)
	if err != nil || none.table.Len() != 0 {
		t.Fatal("one-hop limit kept multi-hop routes", err)
	}
}

func containsPool(keys []domain.PoolKey, key domain.PoolKey) bool {
	for _, k := range keys {
		if k == key {
			return true
		}
	}
	return false
}

// The per-pool cap bounds how many routes the fast lane ranks through a dirty
// pool, keeping the shortest first; the full table still holds them all.
func TestShortEngineCapKeepsShortestRoutesPerPool(t *testing.T) {
	// A hub pool shared by a two-hop and several three-hop cycles.
	a := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	b := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	c := testV2State(3, 2, 3, 1_000_000, 2_000_000)
	d := testV2State(4, 3, 1, 1_000_000, 2_000_000)
	e := testV2State(5, 2, 3, 1_000_000, 2_100_000)
	f := testV2State(6, 3, 1, 1_000_000, 2_100_000)
	states := []pool.State{a, b, c, d, e, f}
	full := mustEngine(t, mustTable(t, 3, states...), 1000, 2, identityQuoter())
	capped, err := full.ShortEngine(t.Context(), 3, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	short, ok := capped.table.(*shortCycles)
	if !ok {
		t.Fatalf("fast table type %T", capped.table)
	}
	// Pool a is in a two-hop cycle (a,b) and three-hop cycles; the cap of 1
	// keeps the two-hop one.
	ids := capped.table.Candidates([]domain.PoolKey{a.Key}, capped.table.Len())
	if len(ids) != 1 {
		t.Fatalf("cap did not bound candidates: %d", len(ids))
	}
	if n := len(short.Cycle(ids[0]).Pools); n != 2 {
		t.Fatalf("cap kept a %d-hop route over the two-hop one", n)
	}
	// Uncapped, the same pool returns every route it is in.
	if uncapped, _ := full.ShortEngine(t.Context(), 3, 2, 0); len(uncapped.table.Candidates([]domain.PoolKey{a.Key}, uncapped.table.Len())) <= 1 {
		t.Fatal("fixture should have multiple routes through the hub pool")
	}
}

// A route the full search found profitable ranks past the per-pool cap until
// hotRouteTTL passes; routes outside the fast lane are ignored.
func TestShortEngineHotRoutesRankPastTheCap(t *testing.T) {
	a := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	b := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	c := testV2State(3, 2, 3, 1_000_000, 2_000_000)
	d := testV2State(4, 3, 1, 1_000_000, 2_000_000)
	full := mustEngine(t, mustTable(t, 3, a, b, c, d), 1000, 2, identityQuoter())
	capped, err := full.ShortEngine(t.Context(), 3, 2, 1)
	if err != nil {
		t.Fatal(err)
	}
	short := capped.table.(*shortCycles)
	var hot graph.Cycle
	for i := range short.Len() {
		if cycle := short.Cycle(i); len(cycle.Pools) == 3 && containsPool(cycle.Pools, a.Key) {
			hot = cycle
			break
		}
	}
	if hot.Pools == nil {
		t.Fatal("fixture has no three-hop route through the hub pool")
	}
	candidates := func() int { return len(capped.table.Candidates([]domain.PoolKey{a.Key}, capped.table.Len())) }
	if candidates() != 1 {
		t.Fatal("cap should keep only the two-hop route")
	}
	dirty := []domain.PoolKey{a.Key}
	if got := capped.FastCoverage(hot, dirty, time.Now()); got != CoverageNotCandidate+"_never_found" {
		t.Fatal("route past the cap classified", got)
	}
	capped.MarkHot(hot, time.Now())
	if candidates() != 2 {
		t.Fatal("a hot route did not rank past the cap")
	}
	if got := capped.FastCoverage(hot, dirty, time.Now()); got != CoverageCandidate {
		t.Fatal("hot route classified", got)
	}
	if got := capped.FastCoverage(graph.Cycle{Pools: []domain.PoolKey{{Address: domain.Address{99}}}}, dirty, time.Now()); got != CoverageNotHeld {
		t.Fatal("unknown route classified", got)
	}
	capped.MarkHot(hot, time.Now().Add(-hotRouteTTL))
	if candidates() != 1 {
		t.Fatal("an expired hot route still ranks")
	}
	capped.MarkHot(graph.Cycle{Start: a.Token0, Pools: []domain.PoolKey{a.Key, {Address: domain.Address{99}}}}, time.Now())
	if candidates() != 1 {
		t.Fatal("a route outside the fast lane was ranked")
	}
}

// A four-hop route is held but ranks only while hot; shorter routes rank as before.
func TestShortEngineRanksLongRoutesOnlyWhenHot(t *testing.T) {
	a := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	b := testV2State(2, 2, 3, 1_000_000, 2_000_000)
	c := testV2State(3, 3, 4, 1_000_000, 2_000_000)
	d := testV2State(4, 4, 1, 1_000_000, 2_000_000)
	full := mustEngine(t, mustTable(t, 4, a, b, c, d), 1000, 2, identityQuoter())
	fast, err := full.ShortEngine(t.Context(), 4, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	short := fast.table.(*shortCycles)
	var four graph.Cycle
	for i := range short.Len() {
		if cycle := short.Cycle(i); len(cycle.Pools) == 4 {
			four = cycle
		}
	}
	if four.Pools == nil {
		t.Fatal("four-hop route not held")
	}
	if n := len(fast.table.Candidates([]domain.PoolKey{a.Key}, fast.table.Len())); n != 0 {
		t.Fatal("a cold four-hop route ranked", n)
	}
	fast.MarkHot(four, time.Now())
	if n := len(fast.table.Candidates([]domain.PoolKey{a.Key}, fast.table.Len())); n != 1 {
		t.Fatal("a hot four-hop route did not rank", n)
	}
}

// A pool keeps at most hotRoutesPerPool hot routes, dropping the oldest.
func TestHotRoutesPerPoolEvictOldest(t *testing.T) {
	hub := domain.PoolKey{Address: domain.Address{1}}
	var cycles []graph.Cycle
	for i := range hotRoutesPerPool + 1 {
		cycles = append(cycles, graph.Cycle{Start: domain.Address{9}, Pools: []domain.PoolKey{hub, {Address: domain.Address{byte(10 + i)}}}, Len: 2})
	}
	table, err := graph.BuildTableFromCycles(cycles)
	if err != nil {
		t.Fatal(err)
	}
	c, err := newShortCycles(t.Context(), table, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	for i, cycle := range cycles {
		c.markHot(cycle, start.Add(time.Duration(i)*time.Second))
	}
	if len(c.hot[hub]) != hotRoutesPerPool {
		t.Fatal("hot routes through the hub", len(c.hot[hub]))
	}
	if _, kept := c.hot[hub][0]; kept {
		t.Fatal("the oldest hot route was kept")
	}
}

// The full search reports a route rotated to start at its funding asset,
// while the fast lane holds it from another start: marking it hot must not
// depend on the rotation.
func TestShortEngineHotRouteMatchesAnyRotation(t *testing.T) {
	states := []pool.State{
		testV2State(1, 1, 2, 1_000_000, 2_000_000),
		testV2State(2, 2, 3, 1_000_000, 2_000_000),
		testV2State(3, 3, 4, 1_000_000, 2_000_000),
		testV2State(4, 4, 1, 1_000_000, 2_000_000),
	}
	full := mustEngine(t, mustTable(t, 4, states...), 1000, 2, identityQuoter())
	fast, err := full.ShortEngine(t.Context(), 4, 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	var four graph.Cycle
	for i := range fast.table.Len() {
		if cycle := fast.table.Cycle(i); len(cycle.Pools) == 4 {
			four = cycle
		}
	}
	if four.Pools == nil {
		t.Fatal("four-hop route not held")
	}
	byKey := map[domain.PoolKey]pool.State{}
	for _, s := range states {
		byKey[s.Key] = s
	}
	first := byKey[four.Pools[0]]
	next := first.Token0
	if next == four.Start {
		next = first.Token1
	}
	rotated := graph.Cycle{Start: next, Pools: append(append([]domain.PoolKey{}, four.Pools[1:]...), four.Pools[0]), Len: 4}
	fast.MarkHot(rotated, time.Now())
	if n := len(fast.table.Candidates([]domain.PoolKey{states[0].Key}, fast.table.Len())); n != 1 {
		t.Fatal("a hot route reported from another start did not rank", n)
	}
}

// A route reported from any start along it is found in the table; a route the
// table does not hold is not.
func TestTableHoldsAnyRotation(t *testing.T) {
	states := []pool.State{
		testV2State(1, 1, 2, 1_000_000, 2_000_000),
		testV2State(2, 2, 3, 1_000_000, 2_000_000),
		testV2State(3, 3, 1, 1_000_000, 2_000_000),
	}
	engine := mustEngine(t, mustTable(t, 3, states...), 1000, 2, identityQuoter())
	view := newFakeView().add(states[0]).add(states[1]).add(states[2])
	held := engine.table.Cycle(0)
	if !engine.TableHolds(view, held) {
		t.Fatal("table route not found")
	}
	first := states[0]
	for _, s := range states {
		if s.Key == held.Pools[0] {
			first = s
		}
	}
	next := first.Token0
	if next == held.Start {
		next = first.Token1
	}
	rotated := graph.Cycle{Start: next, Pools: append(append([]domain.PoolKey{}, held.Pools[1:]...), held.Pools[0]), Len: held.Len}
	if !engine.TableHolds(view, rotated) {
		t.Fatal("rotated table route not found")
	}
	outside := graph.Cycle{Start: held.Start, Pools: []domain.PoolKey{held.Pools[0], {Address: domain.Address{99}}}, Len: 2}
	if engine.TableHolds(view, outside) {
		t.Fatal("route outside the table found")
	}
}

// On a packed, compiled table the fast lane ranks through a subset of the
// shared layout, and ranks exactly as it does through generic traversal.
func TestCompiledShortEngineRanksLikeGenericTraversal(t *testing.T) {
	states := []pool.State{
		testV2State(1, 1, 2, 1_000_000, 2_000_000),
		testV2State(2, 2, 1, 1_000_000, 2_000_000),
		testV2State(3, 2, 1, 1_000_000, 2_500_000),
		testV2State(4, 2, 3, 1_000_000, 2_000_000),
		testV2State(5, 3, 1, 1_000_000, 2_200_000),
	}
	packed, err := graph.PackTable(t.Context(), mustTable(t, 3, states...), states)
	if err != nil {
		t.Fatal(err)
	}
	compiled := mustEngine(t, packed, 1000, 4, identityQuoter())
	if err := compiled.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	generic := mustEngine(t, packed, 1000, 4, identityQuoter())
	fast, err := compiled.ShortEngine(t.Context(), 3, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	slow, err := generic.ShortEngine(t.Context(), 3, 4, 0)
	if err != nil {
		t.Fatal(err)
	}
	if fast.compiled == nil || slow.compiled != nil {
		t.Fatal("compiled fast lane", fast.compiled != nil, slow.compiled != nil)
	}
	view := newFakeView()
	for _, s := range states {
		view.add(s)
	}
	opts := rankedOptions(states[0].Token0, 100000)
	ranked := 0
	for _, s := range states {
		dirty := []domain.PoolKey{s.Key}
		want, err := slow.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := fast.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, got)
		ranked += len(want.candidates)
	}
	if ranked == 0 {
		t.Fatal("fixture ranks no route")
	}
}
