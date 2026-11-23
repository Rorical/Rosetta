package graph

import (
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func pathCycle(start domain.Address, pools ...pool.State) Cycle {
	keys := make([]domain.PoolKey, len(pools))
	for i, state := range pools {
		keys[i] = state.Key
	}
	return Cycle{Pools: keys, Start: start, Len: len(keys)}
}

func TestBuildTableFromCyclesRoundTrip(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	cycles := []Cycle{
		pathCycle(testAddress(0x01), pAB, pBC),
		pathCycle(testAddress(0x01), pBC, pAB),
	}
	table, err := BuildTableFromCycles(cycles)
	if err != nil {
		t.Fatalf("BuildTableFromCycles: %v", err)
	}
	if got := table.Len(); got != 2 {
		t.Fatalf("Len() = %d, want 2", got)
	}
	for i, want := range cycles {
		got := table.Cycle(i)
		if got.Start != want.Start || got.Len != want.Len || !slices.Equal(got.Pools, want.Pools) {
			t.Fatalf("cycle %d = %+v, want %+v", i, got, want)
		}
	}

	// Both cycles contain both pools, so each pool index lists both.
	for _, key := range []domain.PoolKey{pAB.Key, pBC.Key} {
		if got := table.Candidates([]domain.PoolKey{key}, 10); !slices.Equal(got, []int{0, 1}) {
			t.Fatalf("Candidates(%s) = %v, want [0 1]", key.Address, got)
		}
	}
}

func TestBuildTableFromCyclesDropsDuplicatesAndCopiesPools(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	cycle := pathCycle(testAddress(0x01), pAB, pBC)

	table, err := BuildTableFromCycles([]Cycle{cycle, cycle})
	if err != nil {
		t.Fatalf("BuildTableFromCycles: %v", err)
	}
	if got := table.Len(); got != 1 {
		t.Fatalf("Len() = %d, want 1 after dedupe", got)
	}

	cycle.Pools[0] = domain.PoolKey{Address: testAddress(0x99)}
	if got := table.Cycle(0).Pools[0]; got != pAB.Key {
		t.Fatalf("table cycle changed after mutating the input: %+v", got)
	}
}

func TestBuildTableFromCyclesRejectsInvalidCycles(t *testing.T) {
	pAB := testPool(0x10, 0x01, 0x02)
	pBC := testPool(0x20, 0x02, 0x03)
	tests := []struct {
		name  string
		cycle Cycle
	}{
		{name: "one pool", cycle: Cycle{Start: testAddress(0x01), Pools: []domain.PoolKey{pAB.Key}, Len: 1}},
		{name: "no pools", cycle: Cycle{Start: testAddress(0x01)}},
		{name: "len mismatch", cycle: Cycle{Start: testAddress(0x01), Pools: []domain.PoolKey{pAB.Key, pBC.Key}, Len: 3}},
		{name: "repeated pool", cycle: Cycle{
			Start: testAddress(0x01),
			Pools: []domain.PoolKey{pAB.Key, pAB.Key},
			Len:   2,
		}},
		{name: "zero start", cycle: Cycle{Pools: []domain.PoolKey{pAB.Key, pBC.Key}, Len: 2}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, err := BuildTableFromCycles([]Cycle{test.cycle})
			if !errors.Is(err, ErrInvalidCycle) {
				t.Fatalf("error = %v, want ErrInvalidCycle", err)
			}
			if table != nil {
				t.Fatal("table returned with an invalid cycle")
			}
		})
	}
}

func TestMergedTableCombinesSources(t *testing.T) {
	shortPools := []pool.State{
		testPool(0x40, 0x20, 0x21),
		testPool(0x41, 0x21, 0x20),
	}
	short := mustBuildTable(t, mustBuild(t, shortPools...), 2, 10)
	ring := ringPools(6)
	deep := mustBuildDeepTable(t, mustBuild(t, ring...), 6, 100, 10)

	merged, err := NewMergedTable([]CycleSource{short, deep})
	if err != nil {
		t.Fatalf("NewMergedTable: %v", err)
	}
	if got := merged.Len(); got != short.Len()+deep.Len() {
		t.Fatalf("Len() = %d, want %d", got, short.Len()+deep.Len())
	}
	for i := 0; i < short.Len(); i++ {
		if got, want := merged.Cycle(i), short.Cycle(i); !slices.Equal(got.Pools, want.Pools) {
			t.Fatalf("cycle %d = %+v, want %+v", i, got, want)
		}
	}
	for i := 0; i < deep.Len(); i++ {
		got := merged.Cycle(short.Len() + i)
		want := deep.Cycle(i)
		if got.Start != want.Start || got.Len != want.Len || !slices.Equal(got.Pools, want.Pools) {
			t.Fatalf("cycle %d = %+v, want %+v", short.Len()+i, got, want)
		}
	}

	// The deep source's index 0/1 become merged indices 2/3.
	if got := merged.Candidates([]domain.PoolKey{ring[0].Key}, 10); !slices.Equal(got, []int{2, 3}) {
		t.Fatalf("Candidates(ring pool) = %v, want [2 3]", got)
	}
	if got := merged.Candidates([]domain.PoolKey{shortPools[0].Key}, 10); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("Candidates(short pool) = %v, want [0 1]", got)
	}
	if got := merged.Candidates([]domain.PoolKey{shortPools[0].Key, ring[0].Key}, 10); !slices.Equal(got, []int{0, 1, 2, 3}) {
		t.Fatalf("Candidates(both) = %v, want [0 1 2 3]", got)
	}
	if got := merged.Candidates([]domain.PoolKey{shortPools[0].Key}, 1); !slices.Equal(got, []int{0}) {
		t.Fatalf("Candidates cap 1 = %v, want [0]", got)
	}
	if got := merged.Candidates(nil, 10); got != nil {
		t.Fatalf("Candidates(empty) = %v, want nil", got)
	}
}

func TestMergedTableDedupesAcrossSources(t *testing.T) {
	pools := ringPools(6)
	table := mustBuildTable(t, mustBuild(t, pools...), 6, 100)
	merged, err := NewMergedTable([]CycleSource{table, table})
	if err != nil {
		t.Fatalf("NewMergedTable: %v", err)
	}
	if got := merged.Len(); got != table.Len() {
		t.Fatalf("Len() = %d, want %d after dedupe", got, table.Len())
	}
	if got := merged.Candidates([]domain.PoolKey{pools[0].Key}, 10); !slices.Equal(got, []int{0, 1}) {
		t.Fatalf("Candidates() = %v, want [0 1]", got)
	}
}

func TestMergedTableWithPathCacheSource(t *testing.T) {
	pools := ringPools(6)
	path, err := BuildTableFromCycles([]Cycle{pathCycle(testAddress(0x01), pools...)})
	if err != nil {
		t.Fatalf("BuildTableFromCycles: %v", err)
	}
	enumerated := mustBuildTable(t, mustBuild(t, pools...), 4, 100)
	merged, err := NewMergedTable([]CycleSource{enumerated, path})
	if err != nil {
		t.Fatalf("NewMergedTable: %v", err)
	}
	if got := merged.Len(); got != 1 {
		t.Fatalf("Len() = %d, want the one path-cache cycle", got)
	}
	if got := merged.Candidates([]domain.PoolKey{pools[0].Key}, 10); !slices.Equal(got, []int{0}) {
		t.Fatalf("Candidates() = %v, want [0]", got)
	}
}

func TestNewMergedTableRejectsNoSources(t *testing.T) {
	if _, err := NewMergedTable(nil); !errors.Is(err, ErrNilCycleSource) {
		t.Fatalf("error = %v, want ErrNilCycleSource", err)
	}
	if _, err := NewMergedTable([]CycleSource{nil}); !errors.Is(err, ErrNilCycleSource) {
		t.Fatalf("error = %v, want ErrNilCycleSource", err)
	}
}
