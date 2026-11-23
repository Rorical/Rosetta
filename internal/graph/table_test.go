package graph

import (
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestBuildTableTwoPoolCycle(t *testing.T) {
	poolA := testPool(0x10, 0x01, 0x02)
	poolB := testPool(0x20, 0x01, 0x02)
	table := mustBuildTable(t, mustBuild(t, poolA, poolB), 2, 10)

	if got := table.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 directed cycles", got)
	}
	wantDirections := [][]domain.PoolKey{
		{poolA.Key, poolB.Key},
		{poolB.Key, poolA.Key},
	}
	for i, wantPools := range wantDirections {
		cycle := table.Cycle(i)
		if !slices.Equal(cycle.Pools, wantPools) {
			t.Fatalf("cycle %d Pools = %+v, want %+v", i, cycle.Pools, wantPools)
		}
		if cycle.Len != 2 {
			t.Fatalf("cycle %d Len = %d, want 2", i, cycle.Len)
		}
		if cycle.Start != testAddress(0x01) {
			t.Fatalf("cycle %d Start = %s, want %s", i, cycle.Start, testAddress(0x01))
		}
	}
}

func TestBuildTableThreePoolCycle(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCA := testPool(0x30, 0x03, 0x01)
	table := mustBuildTable(t, mustBuild(t, pAB, pBC, pCA), 3, 10)

	if got := table.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 directed cycles", got)
	}
	forward := table.Cycle(0)
	wantForward := []domain.PoolKey{pAB.Key, pBC.Key, pCA.Key}
	if !slices.Equal(forward.Pools, wantForward) {
		t.Fatalf("forward Pools = %+v, want %+v", forward.Pools, wantForward)
	}
	if forward.Start != testAddress(0x01) {
		t.Fatalf("forward Start = %s, want %s", forward.Start, testAddress(0x01))
	}

	reverse := table.Cycle(1)
	wantReverse := []domain.PoolKey{pCA.Key, pBC.Key, pAB.Key}
	if !slices.Equal(reverse.Pools, wantReverse) {
		t.Fatalf("reverse Pools = %+v, want %+v", reverse.Pools, wantReverse)
	}
	if reverse.Start != testAddress(0x01) {
		t.Fatalf("reverse Start = %s, want %s", reverse.Start, testAddress(0x01))
	}
}

func TestBuildTableFourPoolCycle(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCD := testPool(0x30, 0x03, 0x04)
	pDA := testPool(0x40, 0x04, 0x01)
	table := mustBuildTable(t, mustBuild(t, pAB, pBC, pCD, pDA), 4, 10)

	if got := table.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2 directed cycles", got)
	}
	forward := table.Cycle(0)
	wantForward := []domain.PoolKey{pAB.Key, pBC.Key, pCD.Key, pDA.Key}
	if !slices.Equal(forward.Pools, wantForward) {
		t.Fatalf("forward Pools = %+v, want %+v", forward.Pools, wantForward)
	}
	if forward.Len != 4 || forward.Start != testAddress(0x01) {
		t.Fatalf("forward Len/Start = %d/%s", forward.Len, forward.Start)
	}

	reverse := table.Cycle(1)
	wantReverse := []domain.PoolKey{pDA.Key, pCD.Key, pBC.Key, pAB.Key}
	if !slices.Equal(reverse.Pools, wantReverse) {
		t.Fatalf("reverse Pools = %+v, want %+v", reverse.Pools, wantReverse)
	}
	if reverse.Start != testAddress(0x01) {
		t.Fatalf("reverse Start = %s, want %s", reverse.Start, testAddress(0x01))
	}
}

func TestBuildTableDepthLimit(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCA := testPool(0x30, 0x03, 0x01)
	table := mustBuildTable(t, mustBuild(t, pAB, pBC, pCA), 2, 10)

	if got := table.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0 with maxDepth 2 on a 3-cycle graph", got)
	}
}

func TestBuildTableEnumeratesEachDirectedCycleOnce(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCA := testPool(0x30, 0x03, 0x01)
	table := mustBuildTable(t, mustBuild(t, pAB, pBC, pCA), 3, 10)

	for i := 0; i < table.Len(); i++ {
		for j := i + 1; j < table.Len(); j++ {
			left, right := table.Cycle(i), table.Cycle(j)
			if left.Start == right.Start && slices.Equal(left.Pools, right.Pools) {
				t.Fatalf("cycles %d and %d are identical: %+v", i, j, left)
			}
		}
	}
}

func TestBuildTableMixedCycleLengths(t *testing.T) {
	pAB1 := testPool(0x10, 0x01, 0x02)
	pAB2 := testPool(0x15, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCA := testPool(0x30, 0x03, 0x01)
	table := mustBuildTable(t, mustBuild(t, pAB1, pAB2, pBC, pCA), 3, 100)

	if got := table.Len(); got != 6 {
		t.Fatalf("Len() = %d, want 6", got)
	}
	counts := map[int]int{}
	for i := 0; i < table.Len(); i++ {
		cycle := table.Cycle(i)
		counts[cycle.Len]++
	}
	if counts[2] != 2 || counts[3] != 4 {
		t.Fatalf("cycle length counts = %v, want two 2-cycles and four 3-cycles", counts)
	}
}

func TestCyclesFollowGraphEdges(t *testing.T) {
	pAB1 := testPool(0x10, 0x01, 0x02)
	pAB2 := testPool(0x15, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCA := testPool(0x30, 0x03, 0x01)
	g := mustBuild(t, pAB1, pAB2, pBC, pCA)
	table := mustBuildTable(t, g, 3, 100)

	for i := 0; i < table.Len(); i++ {
		cycle := table.Cycle(i)
		current := cycle.Start
		for _, poolKey := range cycle.Pools {
			next, found := outgoing(g, current, poolKey)
			if !found {
				t.Fatalf("cycle %d: no edge from %s via pool %s", i, current, poolKey.Address)
			}
			if compareAddresses(next, cycle.Start) < 0 {
				t.Fatalf("cycle %d visits token %s below start %s", i, next, cycle.Start)
			}
			current = next
		}
		if current != cycle.Start {
			t.Fatalf("cycle %d starts at %s and ends at %s", i, cycle.Start, current)
		}
	}
}

func outgoing(g *Graph, from domain.Address, poolKey domain.PoolKey) (domain.Address, bool) {
	for _, edge := range g.Edges(from) {
		if edge.Pool == poolKey {
			return edge.To, true
		}
	}
	return domain.Address{}, false
}

func TestBuildTableTruncatesAtMaxCycles(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCA := testPool(0x30, 0x03, 0x01)
	g := mustBuild(t, pAB, pBC, pCA)

	table, err := BuildTable(g, 3, 1)
	if !errors.Is(err, ErrTableFull) {
		t.Fatalf("BuildTable() error = %v, want ErrTableFull", err)
	}
	if table == nil {
		t.Fatalf("BuildTable() returned nil table with ErrTableFull")
	}
	if got := table.Len(); got != 1 {
		t.Fatalf("truncated Len() = %d, want 1", got)
	}
	if got := table.Cycle(0).Pools; !slices.Equal(got, []domain.PoolKey{pAB.Key, pBC.Key, pCA.Key}) {
		t.Fatalf("truncated first cycle = %+v", got)
	}

	table, err = BuildTable(g, 3, 2)
	if err != nil {
		t.Fatalf("BuildTable() at exact capacity: %v", err)
	}
	if got := table.Len(); got != 2 {
		t.Fatalf("Len() at exact capacity = %d, want 2", got)
	}
}

func TestCandidates(t *testing.T) {
	pAB1 := testPool(0x10, 0x01, 0x02)
	pAB2 := testPool(0x15, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCA := testPool(0x30, 0x03, 0x01)
	table := mustBuildTable(t, mustBuild(t, pAB1, pAB2, pBC, pCA), 3, 100)

	tests := []struct {
		name  string
		dirty []domain.PoolKey
		k     int
		want  []int
	}{
		{"single pool all cycles", []domain.PoolKey{pAB1.Key}, 10, []int{0, 1, 2, 4}},
		{"per-pool cap", []domain.PoolKey{pBC.Key}, 2, []int{1, 3}},
		{"union and sort", []domain.PoolKey{pAB1.Key, pBC.Key}, 2, []int{0, 1, 3}},
		{"duplicate dirty pools", []domain.PoolKey{pBC.Key, pBC.Key}, 1, []int{1}},
		{"unknown pool", []domain.PoolKey{{Address: testAddress(0x99)}}, 10, nil},
		{"empty dirty", nil, 10, nil},
		{"non-positive k", []domain.PoolKey{pAB1.Key}, 0, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := table.Candidates(test.dirty, test.k); !slices.Equal(got, test.want) {
				t.Fatalf("Candidates() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestBuildTableStoresIndependentPoolSlices(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	pCD := testPool(0x30, 0x03, 0x04)
	pDA1 := testPool(0x40, 0x04, 0x01)
	pDA2 := testPool(0x50, 0x04, 0x01)
	table := mustBuildTable(t, mustBuild(t, pAB, pBC, pCD, pDA1, pDA2), 4, 100)

	// Both cycles share the first three pools, exercising the walk's reused
	// path backing array.
	want := [][]domain.PoolKey{
		{pAB.Key, pBC.Key, pCD.Key, pDA1.Key},
		{pAB.Key, pBC.Key, pCD.Key, pDA2.Key},
	}
	for i, wantPools := range want {
		cycle := table.Cycle(i)
		if !slices.Equal(cycle.Pools, wantPools) {
			t.Fatalf("cycle %d Pools = %+v, want %+v", i, cycle.Pools, wantPools)
		}
	}
}

func TestCycleReturnsCopy(t *testing.T) {
	table := mustBuildTable(t, mustBuild(t, testPool(0x10, 0x01, 0x02), testPool(0x20, 0x01, 0x02)), 2, 10)
	cycle := table.Cycle(0)
	cycle.Pools[0] = domain.PoolKey{Address: testAddress(0x99)}

	if got := table.Cycle(0).Pools[0]; got == cycle.Pools[0] {
		t.Fatalf("mutating Cycle result changed the table")
	}
}

func TestCycleOutOfRangeIsZero(t *testing.T) {
	table := mustBuildTable(t, mustBuild(t, testPool(0x10, 0x01, 0x02), testPool(0x20, 0x01, 0x02)), 2, 10)
	for _, index := range []int{-1, table.Len()} {
		if cycle := table.Cycle(index); cycle.Len != 0 || cycle.Pools != nil || cycle.Start != (domain.Address{}) {
			t.Fatalf("Cycle(%d) = %+v, want zero value", index, cycle)
		}
	}
}

func TestBuildTableRejectsInvalidArguments(t *testing.T) {
	g := mustBuild(t, testPool(0x10, 0x01, 0x02))
	tests := []struct {
		name      string
		graph     *Graph
		maxDepth  int
		maxCycles int
		want      error
	}{
		{"nil graph", nil, 2, 1, ErrNilGraph},
		{"shallow depth", g, 1, 1, ErrInvalidDepth},
		{"zero cycles", g, 2, 0, ErrInvalidMaxCycles},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, err := BuildTable(test.graph, test.maxDepth, test.maxCycles)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildTable() error = %v, want %v", err, test.want)
			}
			if table != nil {
				t.Fatalf("BuildTable() returned a table with an argument error")
			}
		})
	}
}

func TestBuildTableEmptyGraph(t *testing.T) {
	table := mustBuildTable(t, mustBuild(t), 4, 10)
	if got := table.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if got := table.Candidates([]domain.PoolKey{{Address: testAddress(0x01)}}, 10); got != nil {
		t.Fatalf("Candidates() = %v, want nil", got)
	}
}

func TestBuildTableDeterministic(t *testing.T) {
	pools := []pool.State{
		testPool(0x10, 0x01, 0x02),
		testPool(0x15, 0x01, 0x02),
		testPool(0x20, 0x02, 0x03),
		testPool(0x30, 0x03, 0x01),
	}
	first := mustBuildTable(t, mustBuild(t, pools...), 3, 100)

	reversedPools := slices.Clone(pools)
	slices.Reverse(reversedPools)
	second := mustBuildTable(t, mustBuild(t, reversedPools...), 3, 100)

	if first.Len() != second.Len() {
		t.Fatalf("Len() = %d vs %d", first.Len(), second.Len())
	}
	for i := 0; i < first.Len(); i++ {
		left, right := first.Cycle(i), second.Cycle(i)
		if left.Start != right.Start || left.Len != right.Len || !slices.Equal(left.Pools, right.Pools) {
			t.Fatalf("cycle %d differs: %+v vs %+v", i, left, right)
		}
	}

	dirty := []domain.PoolKey{{Address: testAddress(0x01)}, {Address: testAddress(0x02)}}
	if left, right := first.Candidates(dirty, 2), second.Candidates(dirty, 2); !slices.Equal(left, right) {
		t.Fatalf("Candidates() = %v vs %v", left, right)
	}
}
