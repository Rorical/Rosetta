package graph

import (
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/pool"
)

// TestBuildDeepTableRangeFindsOnlyItsBand checks that a ring is discovered by
// the length band that contains it and by no other band.
func TestBuildDeepTableRangeFindsOnlyItsBand(t *testing.T) {
	tests := []struct {
		name     string
		pools    []pool.State
		minDepth int
		maxDepth int
		wantLen  int
		wantLens []int
	}{
		{"six ring in 5..8", ringPools(6), 5, 8, 2, []int{6, 6}},
		{"six ring not in 9..12", ringPools(6), 9, 12, 0, nil},
		{"six ring not in 13..16", ringPools(6), 13, 16, 0, nil},
		{"ten ring in 9..12", ringPools(10), 9, 12, 2, []int{10, 10}},
		{"ten ring not in 5..8", ringPools(10), 5, 8, 0, nil},
		{"ten ring not in 13..16", ringPools(10), 13, 16, 0, nil},
		{"fourteen ring in 13..16", ringPools(14), 13, 16, 2, []int{14, 14}},
		{"fourteen ring not in 9..12", ringPools(14), 9, 12, 0, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, err := BuildDeepTableRange(mustBuild(t, test.pools...), test.minDepth, test.maxDepth, 100, 10)
			if err != nil {
				t.Fatalf("BuildDeepTableRange: %v", err)
			}
			if got := table.Len(); got != test.wantLen {
				t.Fatalf("Len() = %d, want %d", got, test.wantLen)
			}
			for i, wantLen := range test.wantLens {
				cycle := table.Cycle(i)
				if cycle.Len != wantLen || len(cycle.Pools) != wantLen {
					t.Fatalf("cycle %d Len/Pools = %d/%d, want %d", i, cycle.Len, len(cycle.Pools), wantLen)
				}
				if cycle.Start != testAddress(0x01) {
					t.Fatalf("cycle %d Start = %s, want %s", i, cycle.Start, testAddress(0x01))
				}
			}
		})
	}
}

// TestBuildDeepTableRangeSkipsShorterCycles puts a two-pool cycle beside a
// six-pool ring: the 5..8 range must return only the ring, so a merged table
// does not collect the short cycle twice.
func TestBuildDeepTableRangeSkipsShorterCycles(t *testing.T) {
	pools := append(ringPools(6),
		testPool(0x30, 0x20, 0x21),
		testPool(0x31, 0x20, 0x21),
	)
	table, err := BuildDeepTableRange(mustBuild(t, pools...), 5, 8, 100, 10)
	if err != nil {
		t.Fatalf("BuildDeepTableRange: %v", err)
	}
	if got := table.Len(); got != 2 {
		t.Fatalf("Len() = %d, want the two directions of the six-pool ring", got)
	}
	for i := 0; i < table.Len(); i++ {
		if got := table.Cycle(i).Len; got != 6 {
			t.Fatalf("cycle %d Len = %d, want 6", i, got)
		}
	}

	short, err := BuildDeepTableRange(mustBuild(t, pools...), 2, 4, 100, 10)
	if err != nil {
		t.Fatalf("BuildDeepTableRange(2..4): %v", err)
	}
	if got := short.Len(); got != 2 {
		t.Fatalf("2..4 Len() = %d, want the two directions of the two-pool cycle", got)
	}
}

func TestBuildDeepTableRangeTruncatesAtMaxCycles(t *testing.T) {
	g := mustBuild(t, ringPools(6)...)
	table, err := BuildDeepTableRange(g, 5, 8, 1, 10)
	if !errors.Is(err, ErrTableFull) {
		t.Fatalf("BuildDeepTableRange() error = %v, want ErrTableFull", err)
	}
	if table == nil {
		t.Fatal("BuildDeepTableRange() returned nil table with ErrTableFull")
	}
	if got := table.Len(); got != 1 {
		t.Fatalf("truncated Len() = %d, want 1", got)
	}
	if got := table.Cycle(0).Len; got != 6 {
		t.Fatalf("truncated cycle Len = %d, want 6", got)
	}

	table, err = BuildDeepTableRange(g, 5, 8, 2, 10)
	if err != nil {
		t.Fatalf("BuildDeepTableRange() at exact capacity: %v", err)
	}
	if got := table.Len(); got != 2 {
		t.Fatalf("Len() at exact capacity = %d, want 2", got)
	}
}

func TestBuildDeepTableRangeRejectsInvalidArguments(t *testing.T) {
	g := mustBuild(t, testPool(0x10, 0x01, 0x02))
	tests := []struct {
		name      string
		graph     *Graph
		minDepth  int
		maxDepth  int
		maxCycles int
		poolTopK  int
		want      error
	}{
		{"nil graph", nil, 2, 16, 1, 1, ErrNilGraph},
		{"min below two", g, 1, 4, 1, 1, ErrInvalidDepth},
		{"max above sixteen", g, 5, 17, 1, 1, ErrInvalidDepth},
		{"min above sixteen", g, 17, 17, 1, 1, ErrInvalidDepth},
		{"min above max", g, 9, 8, 1, 1, ErrInvalidDepth},
		{"zero cycles", g, 5, 8, 0, 1, ErrInvalidMaxCycles},
		{"zero top k", g, 5, 8, 1, 0, ErrInvalidPoolTopK},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			table, err := BuildDeepTableRange(test.graph, test.minDepth, test.maxDepth, test.maxCycles, test.poolTopK)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildDeepTableRange() error = %v, want %v", err, test.want)
			}
			if table != nil {
				t.Fatal("BuildDeepTableRange() returned a table with an argument error")
			}
		})
	}
}
