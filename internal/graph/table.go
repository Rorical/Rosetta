package graph

import (
	"context"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Cycle is a simple directed cycle over distinct pools. Pools are in traversal
// order beginning at Start, which is the lexicographically smallest token in
// the cycle. Len equals len(Pools).
type Cycle struct {
	Pools []domain.PoolKey
	Start domain.Address
	Len   int
}

// Table is an immutable set of precomputed cycles with a pool-to-cycle index.
type Table struct {
	packed *PathLayout
	cycles []Cycle
	byPool map[domain.PoolKey][]int
}

// minCyclePools is the shortest simple cycle: two pools trading opposite
// directions between the same token pair.
const minCyclePools = 2

// BuildTable enumerates simple directed cycles of 2..maxDepth pools. A pool is
// used at most once per cycle. A cycle is recorded only from its smallest
// token, so each directed cycle has one enumeration origin. Opposite directions
// around the same pools are distinct cycles and are both recorded.
//
// The table is emitted in deterministic enumeration order: start tokens and
// adjacency lists are sorted before the depth-first walk.
//
// When more than maxCycles distinct cycles exist, BuildTable stops after
// maxCycles and returns ErrTableFull together with that partial table. The
// returned table is valid in that case.
func BuildTable(g *Graph, maxDepth int, maxCycles int) (*Table, error) {
	return BuildTableContext(context.Background(), g, maxDepth, maxCycles)
}

// BuildTableContext supports canceling a background topology build while
// preserving BuildTable's deterministic enumeration and truncation contract.
func BuildTableContext(ctx context.Context, g *Graph, maxDepth int, maxCycles int) (*Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g == nil {
		return nil, ErrNilGraph
	}
	if maxDepth < minCyclePools {
		return nil, fmt.Errorf("%w: got %d, want at least %d", ErrInvalidDepth, maxDepth, minCyclePools)
	}
	if maxCycles < 1 {
		return nil, fmt.Errorf("%w: got %d", ErrInvalidMaxCycles, maxCycles)
	}
	builder := newTableBuilder(ctx, g, minCyclePools, maxDepth, maxCycles)
	builder.enumerate()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cycles, truncated := builder.cycles, builder.truncated
	table := &Table{cycles: cycles}
	table.index()
	if truncated {
		return table, ErrTableFull
	}
	return table, nil
}

// BuildTableFromCycles builds a table from an explicit cycle list, such as a
// historical path cache. Each cycle must name at least two distinct pools, have
// Len equal to len(Pools), and start at a non-zero token. Token membership is
// deliberately not checked here: only a caller holding pool state can verify
// that Start belongs to the cycle and that the legs connect. Exact duplicates
// are dropped, and the input order is preserved otherwise.
func BuildTableFromCycles(cycles []Cycle) (*Table, error) {
	table := &Table{cycles: make([]Cycle, 0, len(cycles))}
	seen := make(map[string]struct{}, len(cycles))
	for i, cycle := range cycles {
		if err := validateCycle(cycle); err != nil {
			return nil, fmt.Errorf("cycle %d: %w", i, err)
		}
		key := cycleKey(cycle)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		table.cycles = append(table.cycles, Cycle{
			Pools: slices.Clone(cycle.Pools),
			Start: cycle.Start,
			Len:   cycle.Len,
		})
	}
	table.index()
	return table, nil
}

// validateCycle checks the invariants a caller-supplied cycle must hold. The
// graph is not consulted, so this works for historical paths whose pools may
// since have dropped out of the active set.
func validateCycle(cycle Cycle) error {
	if len(cycle.Pools) < minCyclePools {
		return fmt.Errorf("%w: %d pools, want at least %d", ErrInvalidCycle, len(cycle.Pools), minCyclePools)
	}
	if cycle.Len != len(cycle.Pools) {
		return fmt.Errorf("%w: Len %d does not match %d pools", ErrInvalidCycle, cycle.Len, len(cycle.Pools))
	}
	if cycle.Start == (domain.Address{}) {
		return fmt.Errorf("%w: zero start token", ErrInvalidCycle)
	}
	seen := make(map[domain.PoolKey]struct{}, len(cycle.Pools))
	for _, key := range cycle.Pools {
		if _, exists := seen[key]; exists {
			return fmt.Errorf("%w: pool %s repeats", ErrInvalidCycle, poolKeyLabel(key))
		}
		seen[key] = struct{}{}
	}
	return nil
}

// enumerateCycles runs the shared depth-first walk and reports whether the
// cycle budget was exhausted. Cycles shorter than minDepth are filtered out,
// which lets a range build cover one length band without repeating the shorter
// bands.
func enumerateCycles(g *Graph, minDepth, maxDepth int, maxCycles int) ([]Cycle, bool) {
	builder := newTableBuilder(nil, g, minDepth, maxDepth, maxCycles)
	builder.enumerate()
	return builder.cycles, builder.truncated
}

// Len returns the number of cycles in the table.
func (t *Table) Len() int {
	if t.packed != nil {
		return len(t.packed.Paths)
	}
	return len(t.cycles)
}

// Cycle returns cycle i with a copy of its Pools slice. It returns the zero
// Cycle when i is out of range.
func (t *Table) Cycle(i int) Cycle {
	return t.CycleInto(i, nil)
}

// CycleInto copies cycle i into caller-owned storage, reusing dst's capacity.
// It never exposes the table's immutable pool slice.
func (t *Table) CycleInto(i int, dst []domain.PoolKey) Cycle {
	if t.packed != nil {
		return t.packed.cycleInto(i, dst)
	}
	return cycleInto(t.cycles, i, dst)
}

func cycleInto(cycles []Cycle, i int, dst []domain.PoolKey) Cycle {
	if i < 0 || i >= len(cycles) {
		return Cycle{}
	}
	cycle := cycles[i]
	cycle.Pools = append(dst[:0], cycle.Pools...)
	return cycle
}

// Candidates returns the sorted unique indices of cycles containing at least
// one dirty pool. Each dirty pool contributes at most its first k cycles in
// table order. It returns nil when dirty is empty or k is not positive.
func (t *Table) Candidates(dirty []domain.PoolKey, k int) []int {
	if t.packed != nil {
		var b CandidateBuffer
		return b.Select(t, dirty, k)
	}
	return candidateIDs(t.byPool, dirty, k)
}

func candidateIDs(byPool map[domain.PoolKey][]int, dirty []domain.PoolKey, k int) []int {
	if len(dirty) == 0 || k <= 0 {
		return nil
	}
	selected := make(map[int]struct{})
	for _, key := range dirty {
		indices := byPool[key]
		if k < len(indices) {
			indices = indices[:k]
		}
		for _, index := range indices {
			selected[index] = struct{}{}
		}
	}
	if len(selected) == 0 {
		return nil
	}
	candidates := make([]int, 0, len(selected))
	for index := range selected {
		candidates = append(candidates, index)
	}
	slices.Sort(candidates)
	return candidates
}

func (t *Table) index() {
	t.byPool = make(map[domain.PoolKey][]int)
	for i, cycle := range t.cycles {
		for _, key := range cycle.Pools {
			t.byPool[key] = append(t.byPool[key], i)
		}
	}
}

// poolKeyWireLen is the encoded width of one pool key in a cycle fingerprint:
// address, kind, pool ID, and hooks.
const poolKeyWireLen = len(domain.Address{}) + 1 + len(domain.Hash{}) + len(domain.Address{})

// cycleKey identifies a directed cycle by its start token and traversal-order
// pool sequence. Opposite directions around the same pools have different
// sequences and therefore different keys.
func cycleKey(cycle Cycle) string {
	key := make([]byte, 0, len(cycle.Start)+len(cycle.Pools)*poolKeyWireLen)
	key = append(key, cycle.Start[:]...)
	for _, poolKey := range cycle.Pools {
		key = appendPoolKeyBytes(key, poolKey)
	}
	return string(key)
}

func appendPoolKeyBytes(dst []byte, key domain.PoolKey) []byte {
	dst = append(dst, key.Address[:]...)
	dst = append(dst, byte(key.Kind))
	dst = append(dst, key.PoolID[:]...)
	return append(dst, key.Hooks[:]...)
}
