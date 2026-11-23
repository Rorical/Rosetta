package arb

import (
	"context"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

type compiledPaths struct {
	keys   []domain.PoolKey
	tokens []domain.Address
	pairs  [][2]uint32
	paths  []graph.PackedPath
	hops   []uint32
	layout *graph.PathLayout
}

// PreparePaths shares the runtime table's immutable packed arrays. Generic
// sources compile separately and keep their original candidate-limit semantics.
func (e *Engine) PreparePaths(ctx context.Context, pools []pool.State) error {
	layout, err := graph.CompileLayout(ctx, e.table, pools)
	if err != nil {
		return err
	}
	e.compiled = &compiledPaths{keys: layout.Keys, tokens: layout.Tokens, pairs: layout.Pairs, paths: layout.Paths, hops: layout.Hops, layout: layout}
	return nil
}

// path reads paths and hops directly: a fast lane's paths index the full
// engine's hops and carry no layout of their own.
func (p *compiledPaths) path(index int) []uint32 {
	path := p.paths[index]
	return p.hops[path.Offset : path.Offset+path.Length]
}
