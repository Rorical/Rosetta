package graph

import (
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// referenceWalk is the unpruned map-based enumeration the dense walk replaced.
// Distance pruning and index arithmetic must not change which cycles are
// recorded, their order, or where a budget truncates.
type referenceWalk struct {
	g         *Graph
	maxDepth  int
	maxCycles int
	anchored  bool
	cycles    []Cycle
	seen      map[string]bool
	used      map[domain.PoolKey]bool
	truncated bool
}

func (r *referenceWalk) walk(current, start domain.Address, pools []domain.PoolKey, from []domain.Address, visited map[domain.Address]bool) bool {
	for _, edge := range r.g.adjacency[current] {
		if r.used[edge.Pool] {
			continue
		}
		nextPools := append(slices.Clone(pools), edge.Pool)
		nextFrom := append(slices.Clone(from), current)
		if edge.To == start {
			if len(nextPools) < minCyclePools || !r.anchored && !startsAtSmallest(start, nextFrom) {
				continue
			}
			origin, keys := canonicalCycle(start, nextPools, nextFrom)
			cycle := Cycle{Pools: slices.Clone(keys), Start: origin, Len: len(keys)}
			key := cycleKey(cycle)
			if r.seen[key] {
				continue
			}
			if len(r.cycles) == r.maxCycles {
				r.truncated = true
				return true
			}
			r.seen[key] = true
			r.cycles = append(r.cycles, cycle)
			continue
		}
		if len(nextPools) >= r.maxDepth || visited[edge.To] {
			continue
		}
		r.used[edge.Pool], visited[edge.To] = true, true
		stop := r.walk(edge.To, start, nextPools, nextFrom, visited)
		delete(r.used, edge.Pool)
		delete(visited, edge.To)
		if stop {
			return true
		}
	}
	return false
}

func startsAtSmallest(start domain.Address, tokens []domain.Address) bool {
	for _, token := range tokens {
		if compareAddresses(token, start) < 0 {
			return false
		}
	}
	return true
}

func referenceTable(g *Graph, maxDepth, maxCycles int) ([]Cycle, bool) {
	r := &referenceWalk{g: g, maxDepth: maxDepth, maxCycles: maxCycles, seen: map[string]bool{}, used: map[domain.PoolKey]bool{}}
	for _, start := range g.tokens {
		if r.walk(start, start, nil, nil, map[domain.Address]bool{start: true}) {
			break
		}
	}
	return r.cycles, r.truncated
}

func referenceAnchored(g *Graph, anchor domain.PoolKey, maxDepth, maxCycles int) ([]Cycle, bool) {
	r := &referenceWalk{g: g, maxDepth: maxDepth, maxCycles: maxCycles, anchored: true, seen: map[string]bool{}, used: map[domain.PoolKey]bool{}}
	for _, from := range g.tokens {
		for _, edge := range g.adjacency[from] {
			if edge.Pool != anchor {
				continue
			}
			clear(r.used)
			r.used[edge.Pool] = true
			visited := map[domain.Address]bool{edge.From: true, edge.To: true}
			if r.walk(edge.To, edge.From, []domain.PoolKey{edge.Pool}, []domain.Address{edge.From}, visited) {
				return r.cycles, true
			}
		}
	}
	return r.cycles, false
}

func sameCycles(a, b []Cycle) bool {
	return slices.EqualFunc(a, b, func(x, y Cycle) bool { return cycleKey(x) == cycleKey(y) && x.Len == y.Len })
}

func TestDenseWalkMatchesReferenceOnRecordedOPCatalog(t *testing.T) {
	g := mustBuild(t, recordedOPPools(t)...)
	for _, depth := range []int{2, 3, 4} {
		for _, budget := range []int{1, 500, 20000} {
			want, wantFull := referenceTable(g, depth, budget)
			table, err := BuildTable(g, depth, budget)
			if err != nil && !errors.Is(err, ErrTableFull) {
				t.Fatal(err)
			}
			if !sameCycles(table.cycles, want) || errors.Is(err, ErrTableFull) != wantFull {
				t.Fatalf("depth %d budget %d: table differs from reference (%d vs %d cycles)", depth, budget, table.Len(), len(want))
			}
		}
	}
	// Every fifth pool keeps the unpruned reference affordable under -race.
	keys := g.PoolKeys()
	for i := 0; i < len(keys); i += 5 {
		key := keys[i]
		for _, depth := range []int{2, 6} {
			for _, budget := range []int{1, 16, 49} {
				want, wantFull := referenceAnchored(g, key, depth, budget)
				got, full, err := AnchoredCycles(t.Context(), g, key, depth, budget)
				if err != nil {
					t.Fatal(err)
				}
				if !sameCycles(got, want) || full != wantFull {
					t.Fatalf("anchor %s depth %d budget %d: %d cycles (full %v), reference %d (full %v)", poolKeyLabel(key), depth, budget, len(got), full, len(want), wantFull)
				}
			}
		}
	}
}
