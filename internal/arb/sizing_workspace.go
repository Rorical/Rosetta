package arb

import (
	"math/big"
	"sync"

	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

type amountSample struct {
	input, net big.Int
	ok         bool
}

type sizingWorkspace struct {
	index                               map[[32]byte]int
	samples                             [108]amountSample
	count                               int
	legs                                []Leg
	net, width, third, low, high, upper big.Int
	left, right, ceiling                big.Int
	hasCeiling                          bool
}

var sizingWorkspaces = sync.Pool{New: func() any {
	return &sizingWorkspace{index: make(map[[32]byte]int)}
}}

func amountKey(amount *big.Int) (key [32]byte) {
	amount.FillBytes(key[:]) // Funding bounds have already been checked as uint256.
	return key
}

func (w *sizingWorkspace) release() {
	clear(w.index)
	clear(w.legs)
	w.legs = w.legs[:0]
	w.count, w.hasCeiling = 0, false
	sizingWorkspaces.Put(w)
}

func (e *Engine) monotoneCycle(view View, cycle graph.Cycle) bool {
	proof, ok := e.quoter.(pool.MonotoneQuoter)
	if !ok {
		return false
	}
	for _, key := range cycle.Pools {
		state, ok := view.Pool(key)
		if !ok || !proof.ExactInputMonotone(state) {
			return false
		}
		if needsDeep(key.Kind) {
			deep, ok := view.Deep(key)
			if !ok || ((deep.WindowMin != 0 || deep.WindowMax != 0) && (state.Tick < deep.WindowMin || state.Tick > deep.WindowMax)) {
				return false
			}
		}
	}
	return true
}

// For a monotone output F, F(x)-x <= F(right)-left throughout [left,right].
// This proof needs neither concavity nor a positive grid sample. Failed right
// endpoints provide no bound and must leave the normal refinement in place.
func (w *sizingWorkspace) cannotImprove(best *big.Int) bool {
	index, found := w.index[amountKey(&w.right)]
	if !found || !w.samples[index].ok {
		return false
	}
	sample := &w.samples[index]
	w.upper.Add(&sample.net, &sample.input)
	w.upper.Sub(&w.upper, &w.left)
	return w.upper.Cmp(best) < 0 || (best.Sign() <= 0 && w.upper.Sign() <= 0)
}
