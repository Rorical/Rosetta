package graph

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// ExtendTableContext preserves existing paths and enumerates only cycles
// containing an added pool. Callers must supply an append-only pool catalog;
// removals or identity changes require a full rebuild. The immutable base stays
// usable until the new table and its index are adopted together.
func ExtendTableContext(ctx context.Context, g *Graph, base *Table, added []domain.PoolKey, maxDepth, maxCycles int) (*Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g == nil || base == nil {
		return nil, ErrNilGraph
	}
	if maxDepth < minCyclePools {
		return nil, ErrInvalidDepth
	}
	if maxCycles < base.Len() || maxCycles < 1 {
		return nil, fmt.Errorf("%w: extension budget is below existing path count", ErrInvalidMaxCycles)
	}
	cycles := slices.Clone(base.cycles)
	if base.packed != nil {
		cycles = make([]Cycle, base.Len())
		for i := range cycles {
			cycles[i] = base.Cycle(i)
		}
	}
	builder := newTableBuilder(ctx, g, minCyclePools, maxDepth, maxCycles)
	builder.cycles = cycles
	for _, index := range base.Candidates(added, base.Len()) {
		builder.seen[cycleKey(cycles[index])] = struct{}{}
	}
	builder.walkAnchors(g.anchorMask(added))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	table := &Table{cycles: builder.cycles, byPool: maps.Clone(base.byPool)}
	if base.packed != nil {
		table.index()
		if builder.truncated {
			return table, ErrTableFull
		}
		return table, nil
	}
	// Reuse untouched postings; copy a touched slice before appending so readers
	// on the previous graph version cannot observe a partially extended index.
	touched := make(map[domain.PoolKey]bool)
	for id := base.Len(); id < table.Len(); id++ {
		for _, key := range table.cycles[id].Pools {
			if !touched[key] {
				table.byPool[key] = slices.Clone(table.byPool[key])
				touched[key] = true
			}
			table.byPool[key] = append(table.byPool[key], id)
		}
	}
	if builder.truncated {
		return table, ErrTableFull
	}
	return table, nil
}

// ExtendPackedContext is ExtendTableContext for a packed base: existing paths
// keep their indices, cycles through added pools follow, and the result is the
// packed runtime layout of states. No base route is materialized, so a refresh
// of a large table costs its hops rather than a Cycle per route.
func ExtendPackedContext(ctx context.Context, g *Graph, states []pool.State, base *Table, added []domain.PoolKey, maxDepth, maxCycles int) (*Table, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if g == nil || base == nil || base.packed == nil {
		return nil, ErrNilGraph
	}
	if maxDepth < minCyclePools {
		return nil, ErrInvalidDepth
	}
	if maxCycles < base.Len() || maxCycles < 1 {
		return nil, fmt.Errorf("%w: extension budget is below existing path count", ErrInvalidMaxCycles)
	}
	dense, err := denseFromLayout(ctx, g, base.packed)
	if err != nil {
		return nil, err
	}
	builder := newTableBuilder(ctx, g, minCyclePools, maxDepth, maxCycles)
	builder.dense = dense
	for _, index := range base.Candidates(added, base.Len()) {
		builder.seen[cycleKey(base.Cycle(index))] = struct{}{}
	}
	builder.walkAnchors(g.anchorMask(added))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	layout, err := dense.pack(ctx, g, states)
	if err != nil {
		return nil, err
	}
	if builder.truncated {
		return &Table{packed: layout}, ErrTableFull
	}
	return &Table{packed: layout}, nil
}

// AnchoredCycles enumerates up to maxCycles simple cycles of 2..maxDepth pools
// through one pool. Callers give every anchor its own budget, so a pool in a
// dense core cannot consume the enumeration that later anchors need. The result
// matches ExtendTableContext from an empty table, without building an index.
func AnchoredCycles(ctx context.Context, g *Graph, anchor domain.PoolKey, maxDepth, maxCycles int) ([]Cycle, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if g == nil {
		return nil, false, ErrNilGraph
	}
	if maxDepth < minCyclePools {
		return nil, false, ErrInvalidDepth
	}
	if maxCycles < 1 {
		return nil, false, fmt.Errorf("%w: got %d", ErrInvalidMaxCycles, maxCycles)
	}
	builder := newTableBuilder(ctx, g, minCyclePools, maxDepth, maxCycles)
	// A directed cycle crosses the anchor in exactly one direction, and the
	// walk from that directed edge reaches each simple path once, so one
	// anchor cannot produce a duplicate.
	builder.seen = nil
	builder.walkAnchors(g.anchorMask([]domain.PoolKey{anchor}))
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return builder.cycles, builder.truncated, nil
}

// Anchored discovery can start at any token. Normalize to the same identity as
// a full enumeration so multiple added pools cannot duplicate a single cycle.
func canonicalCycle(start domain.Address, keys []domain.PoolKey, tokens []domain.Address) (domain.Address, []domain.PoolKey) {
	smallest := 0
	for i := 1; i < len(tokens); i++ {
		if compareAddresses(tokens[i], tokens[smallest]) < 0 {
			smallest = i
		}
	}
	if smallest == 0 {
		return start, keys
	}
	return tokens[smallest], append(slices.Clone(keys[smallest:]), keys[:smallest]...)
}
