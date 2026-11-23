package arb

import (
	"container/heap"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// RankedSelection contains at most K candidates per funding asset. It owns no
// state readers; callers must keep the ranking snapshot alive through Optimize.
// Coarse estimates rank candidates and are never execution evidence.
type RankedSelection struct {
	candidates []rankedCycle
	considered int
	issues     []QuoteIssue
}

// Pools returns exactly the dependencies needed by the selected candidates.
func (s RankedSelection) Pools() []domain.PoolKey {
	var keys []domain.PoolKey
	for _, candidate := range s.candidates {
		keys = append(keys, candidate.cycle.Pools...)
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return slices.Compact(keys)
}

// Worst candidate is the root, allowing bounded admission without sorting all
// profitable paths. Equal estimates retain deterministic pool-sequence order.
type rankedHeap []rankedCycle

func (h rankedHeap) Len() int           { return len(h) }
func (h rankedHeap) Less(i, j int) bool { return compareRanked(h[i], h[j]) > 0 }
func (h rankedHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *rankedHeap) Push(value any)    { *h = append(*h, value.(rankedCycle)) }
func (h *rankedHeap) Pop() any {
	last := len(*h) - 1
	value := (*h)[last]
	(*h)[last] = rankedCycle{}
	*h = (*h)[:last]
	return value
}
func (h *rankedHeap) admit(candidate rankedCycle, limit int) {
	if len(*h) < limit {
		heap.Push(h, candidate)
		return
	}
	if compareRanked(candidate, (*h)[0]) < 0 {
		(*h)[0] = candidate
		heap.Fix(h, 0)
	}
}

func compareRanked(a, b rankedCycle) int {
	if order := b.net.Cmp(a.net); order != 0 {
		return order
	}
	return comparePoolSequences(a.cycle.Pools, b.cycle.Pools)
}

// Cycles returns owned path identities for offline ranking diagnostics.
func (s RankedSelection) Cycles() []graph.Cycle {
	out := make([]graph.Cycle, len(s.candidates))
	for i, candidate := range s.candidates {
		out[i] = candidate.cycle
		out[i].Pools = slices.Clone(candidate.cycle.Pools)
	}
	return out
}

func (s RankedSelection) hasRoute(key string) bool {
	for _, candidate := range s.candidates {
		if routeKey(candidate.cycle) == key {
			return true
		}
	}
	return false
}

// position is key's place in the coarse order among candidates that share its
// funding asset, best first; -1 when it was not selected.
func (s RankedSelection) position(key string) int {
	var start domain.Address
	found := false
	for _, candidate := range s.candidates {
		if routeKey(candidate.cycle) == key {
			start, found = candidate.cycle.Start, true
			break
		}
	}
	if !found {
		return -1
	}
	var same []rankedCycle
	for _, candidate := range s.candidates {
		if candidate.cycle.Start == start {
			same = append(same, candidate)
		}
	}
	slices.SortFunc(same, compareRanked)
	for i, candidate := range same {
		if routeKey(candidate.cycle) == key {
			return i
		}
	}
	return -1
}

func (s RankedSelection) route(key string) (rankedCycle, bool) {
	for _, candidate := range s.candidates {
		if routeKey(candidate.cycle) == key {
			return candidate, true
		}
	}
	return rankedCycle{}, false
}
