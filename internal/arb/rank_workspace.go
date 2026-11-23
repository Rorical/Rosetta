package arb

import (
	"math/big"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
)

// A workspace belongs to one Rank invocation. Only admitted candidates are
// copied out; no pooled slice or funding bound escapes in RankedSelection.
type rankWorkspace struct {
	candidates    graph.CandidateBuffer
	path          []domain.PoolKey
	tokens        []domain.Address
	rotations     []domain.PoolKey
	funded        []fundedRotation
	legs          []math.ClosedFormLeg
	input, net    big.Int
	upper         big.Int
	intervals     []modelInterval
	curve         math.CycleEstimator
	segmentCurve  math.CycleEstimator
	localSegments localSegments
	curveReady    bool
	curveOffset   int
}

type fundedRotation struct {
	offset    int
	token     domain.Address
	bounds    InputBounds
	low, high float64 // rankFunding's float enclosures of bounds.Min and Max
}

func (w *rankWorkspace) rotate(cycle graph.Cycle, rotation fundedRotation) rankedCycle {
	keys := w.rotations[rotation.offset*len(cycle.Pools) : (rotation.offset+1)*len(cycle.Pools)]
	copied := copy(keys, cycle.Pools[rotation.offset:])
	copy(keys[copied:], cycle.Pools[:rotation.offset])
	return rankedCycle{cycle: graph.Cycle{Start: rotation.token, Pools: keys, Len: len(keys)}, bounds: rotation.bounds}
}

var rankWorkspaces = sync.Pool{New: func() any { return new(rankWorkspace) }}

func (w *rankWorkspace) cycle(table CycleTable, index int) graph.Cycle {
	if copier, ok := table.(interface {
		CycleInto(int, []domain.PoolKey) graph.Cycle
	}); ok {
		cycle := copier.CycleInto(index, w.path)
		w.path = cycle.Pools
		return cycle
	}
	return table.Cycle(index)
}

func (w *rankWorkspace) resize(depth int) {
	w.tokens = slices.Grow(w.tokens[:0], depth)[:depth]
	w.intervals = slices.Grow(w.intervals[:0], depth)[:depth]
	w.rotations = slices.Grow(w.rotations[:0], depth*depth)[:depth*depth]
	w.funded = w.funded[:0]
	w.curveReady = false
	w.curveOffset = 0
}

func (w *rankWorkspace) release() {
	// Clear references to snapshot reserves and candidate amounts before pooling.
	w.localSegments.reset()
	clear(w.funded[:cap(w.funded)])
	clear(w.legs[:cap(w.legs)])
	if cap(w.path) > 64 || cap(w.tokens) > 64 || cap(w.rotations) > 4096 {
		return
	}
	rankWorkspaces.Put(w)
}

func ownRanked(candidate rankedCycle) rankedCycle {
	candidate.cycle.Pools = slices.Clone(candidate.cycle.Pools)
	candidate.input = new(big.Int).Set(candidate.input)
	candidate.net = new(big.Int).Set(candidate.net)
	candidate.bounds = InputBounds{Min: new(big.Int).Set(candidate.bounds.Min), Max: new(big.Int).Set(candidate.bounds.Max), Floor: candidate.bounds.Floor}
	return candidate
}
