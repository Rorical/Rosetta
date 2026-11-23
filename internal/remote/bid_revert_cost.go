package remote

import "math"

// Revert gas includes early protection failures and expensive late reverts.
// Mean cost is appropriate for expectation; a percentile would hide a costly
// minority. Shrink toward the existing prior while the receipt ring warms up.
// Conditional transactions learn in a ring of their own: many of their
// losses cost nothing.
func (e *bidEstimator) observeRevertGas(used, limit uint64, conditional bool) {
	if used == 0 || limit == 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addRevert(min(float64(used)/float64(limit), 1), conditional)
}

// observeFreeLoss records a conditional send the sequencer dropped: a lost
// race that used no gas.
func (e *bidEstimator) observeFreeLoss() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.addRevert(0, true)
}

func (e *bidEstimator) addRevert(ratio float64, conditional bool) {
	ring, next := &e.revertRatios, &e.revertNext
	if conditional {
		ring, next = &e.conditionalRatios, &e.conditionalNext
	}
	if len(*ring) < gasRatios {
		*ring = append(*ring, ratio)
		return
	}
	(*ring)[*next] = ratio
	*next = (*next + 1) % gasRatios
}

// expectedRevertGasLocked is the gas a lost race is expected to burn. The
// prior is loss when the route's sending shape fixes it (planning.lossGas),
// otherwise the precheck's or half the success gas.
func (e *bidEstimator) expectedRevertGasLocked(limit, success uint64, precheck bool, loss uint64, conditional bool) uint64 {
	prior := float64(success) / 2
	if precheck {
		prior = float64(precheckRevertGas)
	}
	if loss != 0 {
		prior = float64(loss)
	}
	ratios := e.revertRatios
	if conditional {
		ratios = e.conditionalRatios
	}
	sum := prior * gasMinRatios
	for _, ratio := range ratios {
		sum += ratio * float64(limit)
	}
	return min(limit, max(1, uint64(math.Ceil(sum/float64(gasMinRatios+len(ratios))))))
}
