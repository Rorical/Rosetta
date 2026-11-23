package arb

import (
	"context"
	"math/bits"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
)

type compiledSnapshot struct {
	layout     *compiledPaths
	pools      []rankPool
	funding    []rankFunding
	needed     []uint64
	tokens     []bool
	ids        []int
	tokenMarks []uint64
}

var compiledSnapshots = sync.Pool{New: func() any { return new(compiledSnapshot) }}

func (s *compiledSnapshot) prepare(ctx context.Context, layout *compiledPaths, indices []int, view View, opts RankedOptions, workers int) (bool, error) {
	s.layout = layout
	s.pools = slices.Grow(s.pools[:0], len(layout.keys))[:len(layout.keys)]
	s.funding = slices.Grow(s.funding[:0], len(layout.tokens))[:len(layout.tokens)]
	s.tokens = slices.Grow(s.tokens[:0], len(layout.tokens))[:len(layout.tokens)]
	words := (len(layout.keys) + 63) / 64
	s.needed = slices.Grow(s.needed[:0], words)[:words]
	clear(s.needed)
	clear(s.tokens)
	for _, index := range indices {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		for _, hop := range layout.path(index) {
			id := hop / 2
			s.needed[id/64] |= uint64(1) << (id % 64)
		}
	}
	return s.readNeeded(ctx, view, opts, workers)
}

// prepareDirty uses the precomputed pool dependency union; it does not walk
// every hop of every affected path before reading the same few pools.
func (s *compiledSnapshot) prepareDirty(ctx context.Context, layout *compiledPaths, dirty []domain.PoolKey, view View, opts RankedOptions, workers int) (bool, error) {
	s.layout = layout
	s.pools = slices.Grow(s.pools[:0], len(layout.keys))[:len(layout.keys)]
	s.funding = slices.Grow(s.funding[:0], len(layout.tokens))[:len(layout.tokens)]
	s.tokens = slices.Grow(s.tokens[:0], len(layout.tokens))[:len(layout.tokens)]
	clear(s.tokens)
	s.needed = layout.layout.MarkDependencies(dirty, s.needed)
	return s.readNeeded(ctx, view, opts, workers)
}

// parallelSnapshotPools is the fewest needed pools worth preparing on several
// cores: each costs a few microseconds of rational arithmetic.
const parallelSnapshotPools = 64

// readNeeded prepares every needed pool's ranking legs, on up to workers cores.
// Each pool writes only its own slot; the funding of their tokens is captured
// afterwards in token order. Serially, about 760 needed pools take about
// 2.6 ms of a frame before its first ranking batch.
func (s *compiledSnapshot) readNeeded(ctx context.Context, view View, opts RankedOptions, workers int) (bool, error) {
	layout := s.layout
	ids := s.ids[:0]
	for word, marked := range s.needed {
		for marked != 0 {
			ids = append(ids, word*64+bits.TrailingZeros64(marked))
			marked &= marked - 1
		}
	}
	s.ids = ids
	workers = max(1, min(workers, len(ids)/parallelSnapshotPools))
	words := (len(layout.tokens) + 63) / 64
	s.tokenMarks = slices.Grow(s.tokenMarks[:0], workers*words)[:workers*words]
	clear(s.tokenMarks)
	mismatched := make([]bool, workers)
	failures := make([]error, workers)
	prepare := func(w int) {
		marks := s.tokenMarks[w*words : (w+1)*words]
		for _, id := range ids[w*len(ids)/workers : (w+1)*len(ids)/workers] {
			if err := ctx.Err(); err != nil {
				failures[w] = err
				return
			}
			p, ok := view.Pool(layout.keys[id])
			pair := layout.pairs[id]
			if ok && (p.Token0 != layout.tokens[pair[0]] || p.Token1 != layout.tokens[pair[1]]) {
				mismatched[w] = true // A different catalog must use generic traversal.
				return
			}
			s.pools[id] = prepareRankPool(p, ok, opts)
			if ok {
				for _, token := range pair {
					marks[token/64] |= uint64(1) << (token % 64)
				}
			}
		}
	}
	if workers == 1 {
		prepare(0)
	} else {
		var group sync.WaitGroup
		for w := range workers {
			group.Go(func() { prepare(w) })
		}
		group.Wait()
	}
	for w := range workers {
		if failures[w] != nil {
			return false, failures[w]
		}
		if mismatched[w] {
			return false, nil
		}
	}
	for word := range words {
		var marked uint64
		for w := range workers {
			marked |= s.tokenMarks[w*words+word]
		}
		for marked != 0 {
			token := word*64 + bits.TrailingZeros64(marked)
			marked &= marked - 1
			if !s.tokens[token] {
				s.funding[token] = captureFunding(opts, layout.tokens[token])
				s.tokens[token] = true
			}
		}
	}
	return true, nil
}

func (s *compiledSnapshot) release() {
	oversized := len(s.pools) > 4096 || len(s.funding) > 4096
	s.layout = nil
	clear(s.pools)
	clear(s.funding)
	if !oversized {
		compiledSnapshots.Put(s)
	}
}
