package discovery

import (
	"context"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

const (
	// Per-candidate budgets: direct two-pool markets first, then deeper
	// companions. A dense admitted core no longer consumes a shared budget.
	candidateShortCycles = 16
	candidateDeepCycles  = 48
	// Bounds memory when an empty catalog makes every eligible pool a candidate.
	selectionCycleLimit = 200000
)

// marketCycles enumerates cycles through each candidate pool with its own
// budget. Cycles made only of admitted pools need no selection. capped counts
// candidates that filled a budget; full reports the global memory bound.
//
// Per-candidate results come from cache when it holds them. They do not depend
// on the other candidates, so deduplication and the global bound below run on
// every call exactly as for a fresh enumeration and truncate identically.
func marketCycles(ctx context.Context, cache *cycleCache, g *graph.Graph, depth int, candidates []domain.PoolKey) (paths []graph.Cycle, capped int, full bool, err error) {
	seen := map[string]bool{}
	var identity []byte
	add := func(cycles []graph.Cycle, minLen int) {
		for _, cycle := range cycles {
			if cycle.Len < minLen {
				continue
			}
			identity = appendCycleIdentity(identity[:0], cycle)
			if !seen[string(identity)] {
				seen[string(identity)] = true
				paths = append(paths, cycle)
			}
		}
	}
	for _, anchor := range candidates {
		if len(paths) >= selectionCycleLimit {
			return paths, capped, true, nil
		}
		entry, err := cache.anchored(ctx, g, anchor, depth)
		if err != nil {
			return nil, 0, false, err
		}
		short, hit := budgeted(entry.short, candidateShortCycles)
		add(short, 2)
		if depth > 2 {
			deep, deepFull := budgeted(entry.deep, candidateDeepCycles)
			add(deep, 3)
			hit = hit || deepFull
		}
		if hit {
			capped++
		}
	}
	return paths, capped, false, nil
}

func appendCycleIdentity(dst []byte, cycle graph.Cycle) []byte {
	dst = append(dst, cycle.Start[:]...)
	for _, key := range cycle.Pools {
		dst = append(dst, byte(key.Kind))
		dst = append(dst, key.Address[:]...)
		dst = append(dst, key.PoolID[:]...)
		dst = append(dst, key.Hooks[:]...)
	}
	return dst
}

// candidatePools are eligible pools not yet admitted, in key order.
func candidatePools(eligible []domain.PoolKey, pinned map[domain.PoolKey]bool) []domain.PoolKey {
	var out []domain.PoolKey
	for _, key := range eligible {
		if !pinned[key] {
			out = append(out, key)
		}
	}
	slices.SortFunc(out, domain.ComparePoolKeys)
	return out
}
