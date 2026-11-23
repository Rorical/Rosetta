package graph

import (
	"context"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// denseCycles is the builder's compact cycle store: a hop is graph pool*2 + 1
// when the walk enters the pool through its higher token, and each path is a
// range of hops. A four-hop cycle costs 24 bytes instead of a 376-byte Cycle,
// which bounds how many routes a host can build (full Cycles peak near 6 GB
// for about 9M routes).
type denseCycles struct {
	hops  []uint32
	paths []PackedPath
}

// add records the builder's current path rotated to begin at origin.
func (d *denseCycles) add(b *tableBuilder, origin int) {
	n := len(b.pathPools)
	offset := uint32(len(d.hops))
	for i := range n {
		j := (origin + i) % n
		pool := b.pathPools[j]
		hop := uint32(pool) * 2
		if b.pathTokens[j] != b.graph.poolTokens[pool][0] {
			hop++
		}
		d.hops = append(d.hops, hop)
	}
	d.paths = append(d.paths, PackedPath{offset, uint32(n)})
}

// rollback drops the paths recorded since the store held n, with their hops.
func (d *denseCycles) rollback(n int) {
	if n < len(d.paths) {
		d.hops = d.hops[:d.paths[n].Offset]
		d.paths = d.paths[:n]
	}
}

// cycles materializes the store for a plain table.
func (d *denseCycles) cycles(g *Graph) []Cycle {
	out := make([]Cycle, len(d.paths))
	for i, path := range d.paths {
		hops := d.hops[path.Offset : path.Offset+path.Length]
		pools := make([]domain.PoolKey, len(hops))
		for j, hop := range hops {
			pools[j] = g.poolKeys[hop/2]
		}
		first := hops[0]
		out[i] = Cycle{Pools: pools, Start: g.tokens[g.poolTokens[first/2][first%2]], Len: len(hops)}
	}
	return out
}

// pack rewrites the store in place into the runtime layout of states, which
// must name exactly the graph's pools. The store must not be used afterwards.
func (d *denseCycles) pack(ctx context.Context, g *Graph, states []pool.State) (*PathLayout, error) {
	if uint64(len(states)) > uint64(^uint32(0))/2 || uint64(len(d.hops)) > uint64(^uint32(0)) {
		return nil, fmt.Errorf("%w: packed capacity", ErrInvalidCycle)
	}
	sorted := slices.Clone(states)
	slices.SortFunc(sorted, func(a, b pool.State) int { return domain.ComparePoolKeys(a.Key, b.Key) })
	p := &PathLayout{poolIDs: make(map[domain.PoolKey]uint32, len(sorted))}
	if err := p.numberPools(sorted); err != nil {
		return nil, err
	}
	if len(sorted) != len(g.poolKeys) {
		return nil, fmt.Errorf("%w: %d pool states for %d graph pools", ErrInvalidPool, len(sorted), len(g.poolKeys))
	}
	// A layout hop's direction is 1 when the path enters through Token1.
	remap := make([]uint32, len(g.poolKeys))
	flip := make([]uint32, len(g.poolKeys))
	for graphPool, key := range g.poolKeys {
		id, ok := p.poolIDs[key]
		if !ok {
			return nil, fmt.Errorf("%w: graph pool %s has no state", ErrInvalidPool, poolKeyLabel(key))
		}
		lower := g.tokens[g.poolTokens[graphPool][0]]
		switch lower {
		case sorted[id].Token0:
		case sorted[id].Token1:
			flip[graphPool] = 1
		default:
			return nil, fmt.Errorf("%w: pool %s tokens changed", ErrInvalidPool, poolKeyLabel(key))
		}
		remap[graphPool] = id
	}
	for i, hop := range d.hops {
		if i%(1<<16) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		graphPool := hop / 2
		d.hops[i] = remap[graphPool]*2 + (hop%2 ^ flip[graphPool])
	}
	p.Paths, p.Hops = slices.Clip(d.paths), slices.Clip(d.hops)
	d.paths, d.hops = nil, nil
	p.buildIndex()
	p.buildMembership()
	return p, nil
}

// denseFromLayout loads a packed table into the store of a graph that holds
// all of its pools, keeping path order.
func denseFromLayout(ctx context.Context, g *Graph, layout *PathLayout) (*denseCycles, error) {
	graphPools := make([]uint32, len(layout.Keys))
	flip := make([]uint32, len(layout.Keys))
	for id, key := range layout.Keys {
		graphPool, ok := g.poolIndex[key]
		if !ok {
			return nil, fmt.Errorf("%w: pool %s left the graph", ErrInvalidPool, poolKeyLabel(key))
		}
		lower := g.tokens[g.poolTokens[graphPool][0]]
		switch lower {
		case layout.Tokens[layout.Pairs[id][0]]:
		case layout.Tokens[layout.Pairs[id][1]]:
			flip[id] = 1
		default:
			return nil, fmt.Errorf("%w: pool %s tokens changed", ErrInvalidPool, poolKeyLabel(key))
		}
		graphPools[id] = uint32(graphPool)
	}
	d := &denseCycles{hops: make([]uint32, 0, len(layout.Hops)), paths: make([]PackedPath, 0, len(layout.Paths))}
	for index := range layout.Paths {
		if index%(1<<16) == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		offset := uint32(len(d.hops))
		path := layout.Path(index)
		for _, hop := range path {
			id := hop / 2
			d.hops = append(d.hops, graphPools[id]*2+(hop%2^flip[id]))
		}
		d.paths = append(d.paths, PackedPath{offset, uint32(len(path))})
	}
	return d, nil
}
