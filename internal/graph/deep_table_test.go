package graph

import (
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func mustBuildDeepTable(t *testing.T, g *Graph, maxDepth, maxCycles, poolTopK int) *DeepTable {
	t.Helper()
	table, err := BuildDeepTable(g, maxDepth, maxCycles, poolTopK)
	if err != nil {
		t.Fatalf("BuildDeepTable: %v", err)
	}
	return table
}

// ringPools returns n pools forming one ring over n distinct tokens. The ring
// has no shorter cycle: each adjacent token pair is joined by exactly one pool.
func ringPools(n int) []pool.State {
	pools := make([]pool.State, 0, n)
	for i := 1; i <= n; i++ {
		next := i + 1
		if i == n {
			next = 1
		}
		pools = append(pools, testPool(byte(0x10+i), byte(i), byte(next)))
	}
	return pools
}

// rankingPools returns a graph where pool pX belongs to four cycles: two
// 3-cycles that are enumerated before the two 2-cycles. It distinguishes
// ranking by length from ranking by insertion order.
func rankingPools() []pool.State {
	pX := testPool(0x10, 0x01, 0x02)
	pB := testPool(0x11, 0x02, 0x03)
	pC := testPool(0x12, 0x03, 0x01)
	pY := testPool(0x13, 0x02, 0x01)
	return []pool.State{pX, pB, pC, pY}
}

func TestBuildDeepTableSixPoolRing(t *testing.T) {
	g := mustBuild(t, ringPools(6)...)

	shallow := mustBuildDeepTable(t, g, 4, 100, 10)
	if got := shallow.Len(); got != 0 {
		t.Fatalf("depth 4 Len() = %d, want 0 for a 6-pool ring", got)
	}

	deep := mustBuildDeepTable(t, g, 6, 100, 10)
	if got := deep.Len(); got != 2 {
		t.Fatalf("depth 6 Len() = %d, want both directions of the 6-cycle", got)
	}
	cycle := deep.Cycle(0)
	if cycle.Len != 6 || len(cycle.Pools) != 6 {
		t.Fatalf("cycle 0 Len/Pools = %d/%d, want 6/6", cycle.Len, len(cycle.Pools))
	}
	if cycle.Start != testAddress(0x01) {
		t.Fatalf("cycle 0 Start = %s, want %s", cycle.Start, testAddress(0x01))
	}
	if got := deep.Cycle(1).Len; got != 6 {
		t.Fatalf("cycle 1 Len = %d, want 6", got)
	}
}

func TestBuildDeepTablePreservesDirection(t *testing.T) {
	pools := ringPools(6)
	table := mustBuildDeepTable(t, mustBuild(t, pools...), 6, 100, 10)

	wantForward := make([]domain.PoolKey, 0, 6)
	for _, state := range pools {
		wantForward = append(wantForward, state.Key)
	}
	wantReverse := slices.Clone(wantForward)
	slices.Reverse(wantReverse)

	if got := table.Cycle(0).Pools; !slices.Equal(got, wantForward) {
		t.Fatalf("forward Pools = %+v, want %+v", got, wantForward)
	}
	if got := table.Cycle(1).Pools; !slices.Equal(got, wantReverse) {
		t.Fatalf("reverse Pools = %+v, want %+v", got, wantReverse)
	}
}

func TestBuildDeepTableTruncatesAtMaxCycles(t *testing.T) {
	pools := ringPools(6)
	g := mustBuild(t, pools...)

	table, err := BuildDeepTable(g, 6, 1, 10)
	if !errors.Is(err, ErrTableFull) {
		t.Fatalf("BuildDeepTable() error = %v, want ErrTableFull", err)
	}
	if table == nil {
		t.Fatalf("BuildDeepTable() returned nil table with ErrTableFull")
	}
	if got := table.Len(); got != 1 {
		t.Fatalf("truncated Len() = %d, want 1", got)
	}
	if got := table.Cycle(0).Pools; !slices.Equal(got, []domain.PoolKey{
		pools[0].Key, pools[1].Key, pools[2].Key, pools[3].Key, pools[4].Key, pools[5].Key,
	}) {
		t.Fatalf("truncated cycle = %+v", got)
	}
	if got := table.Candidates([]domain.PoolKey{pools[0].Key}, 10); !slices.Equal(got, []int{0}) {
		t.Fatalf("truncated Candidates() = %v, want [0]", got)
	}

	table, err = BuildDeepTable(g, 6, 2, 10)
	if err != nil {
		t.Fatalf("BuildDeepTable() at exact capacity: %v", err)
	}
	if got := table.Len(); got != 2 {
		t.Fatalf("Len() at exact capacity = %d, want 2", got)
	}
}

func TestDeepTableRanksPerPoolByLength(t *testing.T) {
	pools := rankingPools()
	g := mustBuild(t, pools...)
	table := mustBuildDeepTable(t, g, 3, 100, 2)
	pX, pB, pC, pY := pools[0].Key, pools[1].Key, pools[2].Key, pools[3].Key

	wantCycles := [][]domain.PoolKey{
		{pX, pB, pC},
		{pX, pY},
		{pC, pB, pX},
		{pC, pB, pY},
		{pY, pX},
		{pY, pB, pC},
	}
	if got := table.Len(); got != len(wantCycles) {
		t.Fatalf("Len() = %d, want %d", got, len(wantCycles))
	}
	for i, want := range wantCycles {
		if got := table.Cycle(i).Pools; !slices.Equal(got, want) {
			t.Fatalf("cycle %d Pools = %+v, want %+v", i, got, want)
		}
	}

	// pX belongs to cycles 0, 1, 2 and 4. Ranking by length keeps the two
	// 2-cycles (1, 4) and drops the 3-cycles (0, 2).
	if got := table.Candidates([]domain.PoolKey{pX}, 10); !slices.Equal(got, []int{1, 4}) {
		t.Fatalf("Candidates(pX) = %v, want [1 4]", got)
	}
	// pB belongs to four 3-cycles, so insertion order decides.
	if got := table.Candidates([]domain.PoolKey{pB}, 10); !slices.Equal(got, []int{0, 2}) {
		t.Fatalf("Candidates(pB) = %v, want [0 2]", got)
	}

	one := mustBuildDeepTable(t, g, 3, 100, 1)
	if got := one.Candidates([]domain.PoolKey{pX}, 10); !slices.Equal(got, []int{1}) {
		t.Fatalf("topK 1 Candidates(pX) = %v, want [1]", got)
	}
}

func TestDeepTableCandidates(t *testing.T) {
	pools := rankingPools()
	table := mustBuildDeepTable(t, mustBuild(t, pools...), 3, 100, 2)
	pX, pB := pools[0].Key, pools[1].Key

	tests := []struct {
		name  string
		dirty []domain.PoolKey
		k     int
		want  []int
	}{
		{"per-pool indexed cap", []domain.PoolKey{pX}, 10, []int{1, 4}},
		{"per-pool query cap", []domain.PoolKey{pB}, 1, []int{0}},
		{"union and sort", []domain.PoolKey{pX, pB}, 2, []int{0, 1, 2, 4}},
		{"duplicate dirty pools", []domain.PoolKey{pX, pX}, 10, []int{1, 4}},
		{"duplicate pools with query cap", []domain.PoolKey{pB, pB}, 1, []int{0}},
		{"unknown pool", []domain.PoolKey{{Address: testAddress(0x99)}}, 10, nil},
		{"empty dirty", nil, 10, nil},
		{"non-positive k", []domain.PoolKey{pX}, 0, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := table.Candidates(test.dirty, test.k); !slices.Equal(got, test.want) {
				t.Fatalf("Candidates() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestDeepTableCycleReturnsCopy(t *testing.T) {
	table := mustBuildDeepTable(t, mustBuild(t, ringPools(6)...), 6, 100, 10)
	cycle := table.Cycle(0)
	cycle.Pools[0] = domain.PoolKey{Address: testAddress(0x99)}

	if got := table.Cycle(0).Pools[0]; got == cycle.Pools[0] {
		t.Fatalf("mutating Cycle result changed the table")
	}
	for _, index := range []int{-1, table.Len()} {
		if cycle := table.Cycle(index); cycle.Len != 0 || cycle.Pools != nil || cycle.Start != (domain.Address{}) {
			t.Fatalf("Cycle(%d) = %+v, want zero value", index, cycle)
		}
	}
}

func TestDeepTableDeterministic(t *testing.T) {
	pools := rankingPools()
	first := mustBuildDeepTable(t, mustBuild(t, pools...), 3, 100, 2)

	reversedPools := slices.Clone(pools)
	slices.Reverse(reversedPools)
	second := mustBuildDeepTable(t, mustBuild(t, reversedPools...), 3, 100, 2)

	if first.Len() != second.Len() {
		t.Fatalf("Len() = %d vs %d", first.Len(), second.Len())
	}
	for i := 0; i < first.Len(); i++ {
		left, right := first.Cycle(i), second.Cycle(i)
		if left.Start != right.Start || left.Len != right.Len || !slices.Equal(left.Pools, right.Pools) {
			t.Fatalf("cycle %d differs: %+v vs %+v", i, left, right)
		}
	}
	dirty := []domain.PoolKey{pools[0].Key, pools[1].Key}
	if left, right := first.Candidates(dirty, 2), second.Candidates(dirty, 2); !slices.Equal(left, right) {
		t.Fatalf("Candidates() = %v vs %v", left, right)
	}
}

func TestBuildDeepTableAcceptsDepthBounds(t *testing.T) {
	g := mustBuild(t, testPool(0x10, 0x01, 0x02), testPool(0x20, 0x01, 0x02))
	table := mustBuildDeepTable(t, g, 16, 10, 1)
	if got := table.Len(); got != 2 {
		t.Fatalf("Len() at depth 16 = %d, want 2", got)
	}
}

func TestBuildDeepTableRejectsInvalidArguments(t *testing.T) {
	g := mustBuild(t, testPool(0x10, 0x01, 0x02))
	tests := []struct {
		name      string
		graph     *Graph
		maxDepth  int
		maxCycles int
		poolTopK  int
		want      error
	}{
		{"nil graph", nil, 2, 1, 1, ErrNilGraph},
		{"shallow depth", g, 1, 1, 1, ErrInvalidDepth},
		{"deep depth", g, 17, 1, 1, ErrInvalidDepth},
		{"zero cycles", g, 2, 0, 1, ErrInvalidMaxCycles},
		{"negative cycles", g, 2, -1, 1, ErrInvalidMaxCycles},
		{"zero top k", g, 2, 1, 0, ErrInvalidPoolTopK},
		{"negative top k", g, 2, 1, -1, ErrInvalidPoolTopK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, err := BuildDeepTable(test.graph, test.maxDepth, test.maxCycles, test.poolTopK)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildDeepTable() error = %v, want %v", err, test.want)
			}
			if table != nil {
				t.Fatalf("BuildDeepTable() returned a table with an argument error")
			}
		})
	}
}

func TestBuildDeepTableEmptyGraph(t *testing.T) {
	table := mustBuildDeepTable(t, mustBuild(t), 16, 10, 5)
	if got := table.Len(); got != 0 {
		t.Fatalf("Len() = %d, want 0", got)
	}
	if got := table.Candidates([]domain.PoolKey{{Address: testAddress(0x01)}}, 10); got != nil {
		t.Fatalf("Candidates() = %v, want nil", got)
	}
}
