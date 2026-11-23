package arb

import (
	"context"
	"math/big"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

type estimateKey struct{ path, rotation int }

type pathInputs struct {
	revision        uint64
	missing, issues uint32
}

type poolRevision struct {
	approximate          [2]bool
	initialized, present bool
	revision             uint64
	reserves             [4]big.Int
	nilReserves          [4]bool
	fees                 [2]uint32
	errors               [2]string
	errorPresent         [2]bool
}

// One evaluator lane owns the revision index. Shards write distinct negative
// entries and share bounded, synchronized numerical memo tables.
// It never contains tick quotes, state readers or executable plans.
type rankCache struct {
	catalogRank      *rankCache
	batchIndices     []int
	progressCursor   [17]int
	layout           *compiledPaths
	epoch            uint64
	revision         uint64
	fundingRevision  uint64
	pass             uint64
	pools            []poolRevision
	funding          []rankFunding
	negative         []uint64
	estimates        estimateMemo
	segments         segmentMemo
	inputs           []pathInputs
	changes          []poolChange
	rotations        []uint16
	rotationsDirty   bool
	fundingValid     bool
	explorer         *graph.Explorer
	explorerEngine   *Engine
	catalogSelection graph.CandidateBuffer
	observedIDs      graph.CandidateBuffer
	catalogCycles    []graph.Cycle
	poolSet          map[domain.PoolKey]struct{}
}

func (c *rankCache) bind(layout *compiledPaths, epoch uint64) {
	if c.layout == layout {
		// Owned numerical caches survive a lineage change. Every needed pool
		// and funding bound is reread before ranking and compared by value.
		c.epoch = epoch
		return
	}
	c.layout, c.epoch = layout, epoch
	clear(c.progressCursor[:])
	c.revision, c.fundingRevision = 0, 0
	c.pools = slices.Grow(c.pools[:0], len(layout.keys))[:len(layout.keys)]
	c.funding = slices.Grow(c.funding[:0], len(layout.tokens))[:len(layout.tokens)]
	c.negative = slices.Grow(c.negative[:0], len(layout.paths))[:len(layout.paths)]
	clear(c.pools)
	clear(c.funding)
	clear(c.negative)
	c.inputs = make([]pathInputs, len(layout.paths))
	c.rotations = make([]uint16, len(layout.paths))
	c.rotationsDirty = true
	for i, path := range layout.paths {
		c.inputs[i].missing = path.Length
	}
	c.estimates.reset()
	c.segments.reset()
}

// parallelCachePostings is the fewest changed-pool postings, or paths for a
// rotation rebuild, worth spreading over cores.
const parallelCachePostings = 1 << 15

// poolChange is one needed pool whose ranking inputs moved this frame.
type poolChange struct {
	id                   uint32
	revision             uint64
	wasPresent, present  bool
	hadErrors, hasErrors [2]bool
}

func (c *rankCache) update(ctx context.Context, s *compiledSnapshot, workers int) error {
	return c.updateWorkers(ctx, s, workers, parallelCachePostings)
}

// updateWorkers advances the revisions of changed pools and of every path
// through them, on up to workers cores. Pools are compared in parallel, take
// revisions serially in ID order, and paths are updated by workers owning
// disjoint index ranges, each applying the changes in ID order, so every path
// ends with the revision and counts a serial update gives. Serially this takes
// 3 ms (p50) to 9 ms (p90) of a frame on a complete multi-million-route table
// before its first ranking batch.
func (c *rankCache) updateWorkers(ctx context.Context, s *compiledSnapshot, workers, minimum int) error {
	c.pass++
	ids := s.ids
	compareWorkers := max(1, min(workers, len(ids)/parallelSnapshotPools))
	found := make([][]poolChange, compareWorkers)
	failures := make([]error, compareWorkers)
	compare := func(w int) {
		for _, id := range ids[w*len(ids)/compareWorkers : (w+1)*len(ids)/compareWorkers] {
			if err := ctx.Err(); err != nil {
				failures[w] = err
				return
			}
			was := &c.pools[id]
			change := poolChange{id: uint32(id), wasPresent: was.initialized && was.present, hadErrors: was.errorPresent}
			if was.update(s.pools[id]) {
				change.present = s.pools[id].ok
				change.hasErrors = [2]bool{s.pools[id].errs[0] != nil, s.pools[id].errs[1] != nil}
				found[w] = append(found[w], change)
			}
		}
	}
	runParts(compareWorkers, compare)
	for _, err := range failures {
		if err != nil {
			return err
		}
	}
	changes := c.changes[:0]
	postings := 0
	for _, part := range found {
		for _, change := range part {
			c.revision++
			change.revision = c.revision
			c.pools[change.id].revision = c.revision
			changes = append(changes, change)
			postings += c.layout.layout.PoolPostings(change.id)
		}
	}
	c.changes = changes
	pathWorkers := 1
	if postings >= minimum {
		pathWorkers = max(1, workers)
	}
	paths := len(c.layout.paths)
	runParts(pathWorkers, func(w int) {
		lo, hi := w*paths/pathWorkers, (w+1)*paths/pathWorkers
		for _, change := range changes {
			c.layout.layout.VisitPoolPathsIn(change.id, lo, hi, func(index int, direction uint32) {
				input := &c.inputs[index]
				input.revision = change.revision
				if change.wasPresent && !change.present {
					input.missing++
				}
				if !change.wasPresent && change.present {
					input.missing--
				}
				if change.hadErrors[direction] {
					input.issues--
				}
				if change.hasErrors[direction] {
					input.issues++
				}
			})
		}
	})
	if err := ctx.Err(); err != nil {
		return err
	}
	for token, needed := range s.tokens {
		if needed && !sameFunding(c.funding[token], s.funding[token]) {
			if c.funding[token].ok != s.funding[token].ok {
				c.rotationsDirty = true
			}
			c.revision++
			c.fundingRevision = c.revision
			c.funding[token] = cloneFunding(s.funding[token])
		}
	}
	c.fundingValid = true
	for _, funding := range c.funding {
		b := funding.bounds
		if funding.ok && (b.Min == nil || b.Max == nil || b.Min.Sign() <= 0 || b.Min.Cmp(b.Max) > 0 || b.Max.BitLen() > 256) {
			c.fundingValid = false
			break
		}
	}
	if c.rotationsDirty {
		rotationWorkers := 1
		if paths >= minimum {
			rotationWorkers = max(1, workers)
		}
		runParts(rotationWorkers, func(w int) {
			for index := w * paths / rotationWorkers; index < (w+1)*paths/rotationWorkers; index++ {
				if index%512 == 0 && ctx.Err() != nil {
					return
				}
				var mask uint16
				path := c.layout.path(index)
				for offset, hop := range path {
					token := c.layout.pairs[hop/2][hop%2]
					if offset < 16 && c.funding[token].ok {
						mask |= uint16(1) << uint(offset)
					}
				}
				c.rotations[index] = mask
			}
		})
		if err := ctx.Err(); err != nil {
			return err
		}
		c.rotationsDirty = false
	}
	return nil
}

// runParts runs part(0..n-1), concurrently when n > 1.
func runParts(n int, part func(int)) {
	if n <= 1 {
		part(0)
		return
	}
	var group sync.WaitGroup
	for w := range n {
		group.Go(func() { part(w) })
	}
	group.Wait()
}

func (p *poolRevision) update(value rankPool) bool {
	reserves := [4]*big.Int{value.legs[0].model.InReserve, value.legs[0].model.OutReserve, value.legs[1].model.InReserve, value.legs[1].model.OutReserve}
	fees := [2]uint32{value.legs[0].model.FeePips, value.legs[1].model.FeePips}
	var failures [2]string
	var errorPresent [2]bool
	for i, err := range value.errs {
		if err != nil {
			errorPresent[i] = true
			failures[i] = err.Error()
		}
	}
	approximate := [2]bool{value.legs[0].approximate, value.legs[1].approximate}
	changed := p.approximate != approximate || !p.initialized || p.present != value.ok || p.fees != fees || p.errors != failures || p.errorPresent != errorPresent
	for i, reserve := range reserves {
		if p.nilReserves[i] != (reserve == nil) || (reserve != nil && p.reserves[i].Cmp(reserve) != 0) {
			changed = true
		}
	}
	if !changed {
		return false
	}
	p.approximate = approximate
	p.initialized, p.present, p.fees, p.errors = true, value.ok, fees, failures
	p.errorPresent = errorPresent
	for i, reserve := range reserves {
		p.nilReserves[i] = reserve == nil
		if reserve != nil {
			p.reserves[i].Set(reserve)
		}
	}
	return true
}

func sameFunding(a, b rankFunding) bool {
	return a.ok == b.ok && (!a.ok || (sameAmount(a.bounds.Min, b.bounds.Min) && sameAmount(a.bounds.Max, b.bounds.Max)))
}

func sameAmount(a, b *big.Int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Cmp(b) == 0
}

func cloneFunding(value rankFunding) rankFunding {
	if value.bounds.Min != nil {
		value.bounds.Min = new(big.Int).Set(value.bounds.Min)
	}
	if value.bounds.Max != nil {
		value.bounds.Max = new(big.Int).Set(value.bounds.Max)
	}
	return value
}
