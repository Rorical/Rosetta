package graph

import (
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// CycleSource is a read-only cycle store that a MergedTable can combine.
// Table, DeepTable, and a path-cache table built by BuildTableFromCycles all
// satisfy it.
type CycleSource interface {
	Len() int
	Cycle(i int) Cycle
	Candidates(dirty []domain.PoolKey, k int) []int
}

var (
	_ CycleSource = (*Table)(nil)
	_ CycleSource = (*DeepTable)(nil)
	_ CycleSource = (*MergedTable)(nil)
)

// MergedTable is the deduplicated union of several cycle sources. Cycles keep
// source order, so indices are stable and deterministic. Candidates asks every
// source for its own candidates, applies that source's offset, and returns the
// sorted unique union. Delegating the per-source query preserves the DeepTable
// index cap: a merge cannot re-admit cycles the source dropped.
type MergedTable struct {
	cycles  []Cycle
	sources []CycleSource
	// remap[s][i] is the merged index of source s's cycle i, or -1 when that
	// cycle duplicates one from an earlier source.
	remap [][]int
}

// NewMergedTable combines sources into one table, dropping cycles that repeat
// an earlier source's start token and pool sequence. At least one non-nil
// source is required.
func NewMergedTable(sources []CycleSource) (*MergedTable, error) {
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: no sources", ErrNilCycleSource)
	}
	merged := &MergedTable{
		sources: slices.Clone(sources),
		remap:   make([][]int, len(sources)),
	}
	seen := make(map[string]struct{})
	for i, source := range sources {
		if source == nil {
			return nil, fmt.Errorf("%w: source %d", ErrNilCycleSource, i)
		}
		count := source.Len()
		mapping := make([]int, count)
		for j := 0; j < count; j++ {
			cycle := source.Cycle(j)
			key := cycleKey(cycle)
			if _, exists := seen[key]; exists {
				mapping[j] = -1
				continue
			}
			seen[key] = struct{}{}
			mapping[j] = len(merged.cycles)
			merged.cycles = append(merged.cycles, cycle)
		}
		merged.remap[i] = mapping
	}
	return merged, nil
}

// Len returns the number of distinct cycles in the merged table.
func (t *MergedTable) Len() int {
	return len(t.cycles)
}

// Cycle returns cycle i with a copy of its Pools slice. It returns the zero
// Cycle when i is out of range.
func (t *MergedTable) Cycle(i int) Cycle {
	return t.CycleInto(i, nil)
}

// CycleInto copies cycle i into caller-owned storage, reusing dst's capacity.
func (t *MergedTable) CycleInto(i int, dst []domain.PoolKey) Cycle {
	return cycleInto(t.cycles, i, dst)
}

// Candidates returns the sorted unique merged indices of cycles containing at
// least one dirty pool. Each source applies its own per-pool cap k before the
// offsets are applied. It returns nil when dirty is empty or k is not positive.
func (t *MergedTable) Candidates(dirty []domain.PoolKey, k int) []int {
	if len(dirty) == 0 || k <= 0 {
		return nil
	}
	selected := make(map[int]struct{})
	for i, source := range t.sources {
		for _, index := range source.Candidates(dirty, k) {
			if index < 0 || index >= len(t.remap[i]) {
				continue
			}
			mapped := t.remap[i][index]
			if mapped >= 0 {
				selected[mapped] = struct{}{}
			}
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
