package graph

import (
	"cmp"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Bounds on the cycle length a DeepTable enumerates. The upper bound keeps the
// depth-first walk and its per-branch path slices bounded.
const (
	minDeepDepth = 2
	maxDeepDepth = 16
)

// DeepTable is an immutable set of precomputed cycles of 2..maxDepth pools
// together with a bounded pool-to-cycle index. Unlike Table, the index keeps
// only the poolTopK shortest cycles per pool, so a deep enumeration cannot
// flood the candidate set of a dirty pool.
type DeepTable struct {
	cycles []Cycle
	byPool map[domain.PoolKey][]int
}

// BuildDeepTable enumerates simple directed cycles of 2..maxDepth pools. It is
// the full-range form of BuildDeepTableRange.
//
// When more than maxCycles distinct cycles exist, BuildDeepTable stops after
// maxCycles and returns ErrTableFull together with that partial table. The
// returned table is valid in that case.
func BuildDeepTable(g *Graph, maxDepth, maxCycles, poolTopK int) (*DeepTable, error) {
	return BuildDeepTableRange(g, minDeepDepth, maxDepth, maxCycles, poolTopK)
}

// BuildDeepTableRange enumerates simple directed cycles of minDepth..maxDepth
// pools. A pool is used at most once per cycle. A cycle is recorded only from
// its smallest token, so each directed cycle has one enumeration origin;
// opposite directions around the same pools are distinct cycles and both are
// recorded.
//
// Cycles shorter than minDepth are not enumerated, so callers can cover 2..4,
// 5..8, 9..12, and 13..16 with separate ranges and merge the results. 13..16 is
// the deepest supported range: enumeration beyond sixteen pools is not
// implemented, and 7–14 or 14+ historical paths are supplied through an
// explicit path cache instead.
//
// Cycles are emitted in deterministic enumeration order, the same order
// BuildTable uses. Each pool index lists at most poolTopK cycles, ranked by
// cycle length and then enumeration order.
//
// When more than maxCycles distinct cycles exist, BuildDeepTableRange stops
// after maxCycles and returns ErrTableFull together with that partial table.
// The returned table is valid in that case.
func BuildDeepTableRange(g *Graph, minDepth, maxDepth, maxCycles, poolTopK int) (*DeepTable, error) {
	if g == nil {
		return nil, ErrNilGraph
	}
	if minDepth < minDeepDepth || minDepth > maxDeepDepth {
		return nil, fmt.Errorf("%w: min depth %d, want %d..%d",
			ErrInvalidDepth, minDepth, minDeepDepth, maxDeepDepth)
	}
	if maxDepth < minDeepDepth || maxDepth > maxDeepDepth {
		return nil, fmt.Errorf("%w: max depth %d, want %d..%d",
			ErrInvalidDepth, maxDepth, minDeepDepth, maxDeepDepth)
	}
	if minDepth > maxDepth {
		return nil, fmt.Errorf("%w: min depth %d exceeds max depth %d", ErrInvalidDepth, minDepth, maxDepth)
	}
	if maxCycles < 1 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxCycles, maxCycles)
	}
	if poolTopK < 1 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidPoolTopK, poolTopK)
	}
	cycles, truncated := enumerateCycles(g, minDepth, maxDepth, maxCycles)
	table := &DeepTable{cycles: cycles}
	table.index(poolTopK)
	if truncated {
		return table, ErrTableFull
	}
	return table, nil
}

// Len returns the number of cycles in the table.
func (t *DeepTable) Len() int {
	return len(t.cycles)
}

// Cycle returns cycle i with a copy of its Pools slice. It returns the zero
// Cycle when i is out of range.
func (t *DeepTable) Cycle(i int) Cycle {
	return t.CycleInto(i, nil)
}

// CycleInto copies cycle i into caller-owned storage, reusing dst's capacity.
func (t *DeepTable) CycleInto(i int, dst []domain.PoolKey) Cycle {
	return cycleInto(t.cycles, i, dst)
}

// Candidates returns the sorted unique indices of cycles containing at least
// one dirty pool. Each dirty pool contributes at most its first k indexed
// cycles. It returns nil when dirty is empty or k is not positive.
func (t *DeepTable) Candidates(dirty []domain.PoolKey, k int) []int {
	return candidateIDs(t.byPool, dirty, k)
}

// index builds the pool-to-cycle lists and truncates each to topK. Cycle IDs
// are appended in enumeration order, so sorting by length and then ID ranks
// shorter cycles first and breaks ties by insertion order regardless of map
// iteration order.
func (t *DeepTable) index(topK int) {
	t.byPool = make(map[domain.PoolKey][]int)
	for id, cycle := range t.cycles {
		for _, key := range cycle.Pools {
			t.byPool[key] = append(t.byPool[key], id)
		}
	}
	for key, ids := range t.byPool {
		slices.SortFunc(ids, func(a, b int) int {
			if diff := cmp.Compare(t.cycles[a].Len, t.cycles[b].Len); diff != 0 {
				return diff
			}
			return cmp.Compare(a, b)
		})
		if len(ids) > topK {
			// Clone the kept prefix so the full list's backing array can be
			// collected; deep tables are the ones where that memory matters.
			ids = slices.Clone(ids[:topK])
		}
		t.byPool[key] = ids
	}
}
