package graph

import (
	"context"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// BuildBalancedTableContext completes each route length while it fits,
// shortest first, so a budget that holds every two- and three-hop route keeps
// all of them (a global DFS fill can keep as few as a fifth of the three-hop
// routes, crowded out by four-hop ones). Lengths that do not fit are first
// seeded per pool and direction, so every pool keeps routes of each length,
// including a new market reachable only through long routes; the rest of the
// budget then fills shortest first. This is bounded coverage, not exhaustive
// enumeration, when ErrTableFull is returned.
func BuildBalancedTableContext(ctx context.Context, g *Graph, maxDepth, maxCycles int) (*Table, error) {
	dense, full, err := buildBalanced(ctx, g, maxDepth, maxCycles)
	if err != nil {
		return nil, err
	}
	table := &Table{cycles: dense.cycles(g)}
	table.index()
	if full {
		return table, ErrTableFull
	}
	return table, nil
}

// BuildPackedBalancedContext builds the BuildBalancedTableContext table
// straight into the packed runtime layout of states, the pools g was built
// from, without materializing a Cycle per route.
func BuildPackedBalancedContext(ctx context.Context, g *Graph, states []pool.State, maxDepth, maxCycles int) (*Table, error) {
	dense, full, err := buildBalanced(ctx, g, maxDepth, maxCycles)
	if err != nil {
		return nil, err
	}
	layout, err := dense.pack(ctx, g, states)
	if err != nil {
		return nil, err
	}
	if full {
		return &Table{packed: layout}, ErrTableFull
	}
	return &Table{packed: layout}, nil
}

// buildBalanced reports full when cycles beyond maxCycles were left out.
func buildBalanced(ctx context.Context, g *Graph, maxDepth, maxCycles int) (*denseCycles, bool, error) {
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
		return nil, false, ErrInvalidMaxCycles
	}
	b := newTableBuilder(ctx, g, 0, 0, maxCycles)
	// A complete length is enumerated once from each cycle's smallest token,
	// so it needs no deduplication set.
	b.dense, b.seen = &denseCycles{}, nil
	depthLimit := min(maxDepth, len(g.tokens))
	depth := minCyclePools
	for ; depth <= depthLimit; depth++ {
		complete, err := b.completeLength(depth)
		if err != nil {
			return nil, false, err
		}
		if !complete {
			break
		}
	}
	if depth > depthLimit {
		return b.dense, false, nil
	}
	short := b.dense.paths
	b.dense.paths, b.seen = nil, make(map[string]struct{})
	quota := min(32, max(1, (maxCycles-len(short))/(2*g.poolCount*(depthLimit-depth+1))))
	b.seed(depth, depthLimit, maxCycles, quota)
	if room := maxCycles - b.count(); len(short) > room {
		short = short[:room]
	}
	b.dense.paths = append(short[:len(short):len(short)], b.dense.paths...)
	// All seeded identities share the deduplication set. Overflow attempts were
	// not inserted into seen, so the fill can still admit those cycles.
	b.maxCycles = maxCycles
	for d := depth; d <= depthLimit && b.count() < maxCycles; d++ {
		b.minDepth, b.maxDepth, b.truncated = d, d, false
		b.enumerate()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	// Length depth alone overflowed the budget left by shorter ones.
	return b.dense, true, nil
}

// completeLength records every cycle of depth pools in the order a serial
// enumeration gives, walking each start token's first pools on all cores. It
// records nothing and reports false when the cycles would exceed the budget.
func (b *tableBuilder) completeLength(depth int) (bool, error) {
	type task struct {
		start int32
		first link
	}
	var tasks []task
	for start, links := range b.graph.links {
		for _, first := range links {
			// Tokens below the start belong to an earlier start's walk.
			if first.to > int32(start) {
				tasks = append(tasks, task{int32(start), first})
			}
		}
	}
	room := b.maxCycles - b.count()
	ctx, overflow := context.WithCancel(b.ctx)
	defer overflow()
	chunks := make([]denseCycles, len(tasks))
	var next, recorded atomic.Int64
	var full atomic.Bool
	var group sync.WaitGroup
	for range min(len(tasks), max(1, runtime.GOMAXPROCS(0))) {
		group.Go(func() {
			w := newTableBuilder(ctx, b.graph, depth, depth, room)
			w.seen = nil
			for i := int(next.Add(1) - 1); i < len(tasks) && ctx.Err() == nil; i = int(next.Add(1) - 1) {
				t := tasks[i]
				w.dense = &chunks[i]
				w.measure(t.start, -1, t.start)
				if 1+int(w.distance[t.first.to]) > depth {
					continue
				}
				w.visited[t.start], w.visited[t.first.to], w.usedPools[t.first.pool] = true, true, true
				w.push(t.first.pool, t.start)
				w.walk(t.first.to, t.start)
				w.pop()
				w.visited[t.start], w.visited[t.first.to], w.usedPools[t.first.pool] = false, false, false
				if w.truncated || recorded.Add(int64(len(chunks[i].paths))) > int64(room) {
					full.Store(true)
					overflow()
				}
			}
		})
	}
	group.Wait()
	if err := b.ctx.Err(); err != nil {
		return false, err
	}
	if full.Load() {
		return false, nil
	}
	total := 0
	for _, chunk := range chunks {
		total += len(chunk.hops)
	}
	b.dense.hops = slices.Grow(b.dense.hops, total)
	for _, chunk := range chunks {
		base := uint32(len(b.dense.hops))
		b.dense.hops = append(b.dense.hops, chunk.hops...)
		for _, path := range chunk.paths {
			b.dense.paths = append(b.dense.paths, PackedPath{path.Offset + base, path.Length})
		}
	}
	return true, nil
}

// seed walks quota cycles from both directions of every pool at each length
// in [from, to], in pool key order, until maxCycles.
func (b *tableBuilder) seed(from, to, maxCycles, quota int) {
	g := b.graph
	// Each pool once, from its smaller token; token indices order like addresses.
	type anchorEdge struct {
		from int32
		link link
	}
	var anchors []anchorEdge
	for from, links := range g.links {
		for _, next := range links {
			if int32(from) < next.to {
				anchors = append(anchors, anchorEdge{from: int32(from), link: next})
			}
		}
	}
	slices.SortFunc(anchors, func(a, b anchorEdge) int {
		return domain.ComparePoolKeys(g.poolKeys[a.link.pool], g.poolKeys[b.link.pool])
	})
	for depth := from; depth <= to && b.count() < maxCycles; depth++ {
		b.minDepth, b.maxDepth = depth, depth
		for _, anchor := range anchors {
			for direction := 0; direction < 2 && b.count() < maxCycles; direction++ {
				if b.ctx.Err() != nil {
					return
				}
				from, next := anchor.from, anchor.link
				if direction == 1 {
					from, next = next.to, link{pool: next.pool, to: from}
				}
				b.maxCycles = min(maxCycles, b.count()+quota)
				b.truncated = false
				b.walkAnchor(from, next)
			}
			if b.count() == maxCycles {
				break
			}
		}
	}
}
