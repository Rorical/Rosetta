package graph

import (
	"context"

	"github.com/Rorical/Rosetta/internal/domain"
)

// One resumable DFS for an anchor/direction/exact-length band. Every examined
// edge and backtrack consumes work, including attempts yielding no cycle.
type exploreCursor struct {
	anchor Edge
	depth  int
	path   []Edge
	next   []int
}

func newExploreCursor(anchor Edge, depth int) *exploreCursor {
	return &exploreCursor{anchor: anchor, depth: depth, path: []Edge{anchor}, next: []int{0}}
}

func (c *exploreCursor) advance(ctx context.Context, g *Graph, budget int, emit func(Cycle) bool) (int, bool, error) {
	steps := 0
	for len(c.path) > 0 && steps < budget {
		if err := ctx.Err(); err != nil {
			return steps, false, err
		}
		level := len(c.path) - 1
		current := c.path[level].To
		edges := g.adjacency[current]
		steps++
		if c.next[level] == len(edges) {
			c.path = c.path[:level]
			c.next = c.next[:level]
			continue
		}
		edge := edges[c.next[level]]
		c.next[level]++
		used := false
		for _, prior := range c.path {
			if prior.Pool == edge.Pool {
				used = true
				break
			}
		}
		if used {
			continue
		}
		if edge.To == c.anchor.From {
			if len(c.path)+1 != c.depth {
				continue
			}
			keys := make([]domain.PoolKey, 0, c.depth)
			tokens := make([]domain.Address, 0, c.depth)
			for _, prior := range c.path {
				keys = append(keys, prior.Pool)
				tokens = append(tokens, prior.From)
			}
			keys = append(keys, edge.Pool)
			tokens = append(tokens, edge.From)
			start, keys := canonicalCycle(c.anchor.From, keys, tokens)
			if emit(Cycle{Start: start, Pools: keys, Len: len(keys)}) {
				return steps, false, nil
			}
			continue
		}
		if len(c.path)+1 >= c.depth {
			continue
		}
		for _, prior := range c.path {
			if prior.From == edge.To {
				used = true
				break
			}
		}
		if used {
			continue
		}
		c.path = append(c.path, edge)
		c.next = append(c.next, 0)
	}
	return steps, len(c.path) == 0, nil
}
