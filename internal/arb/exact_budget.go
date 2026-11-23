package arb

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// exactLimit is the per-asset amount-search budget of one job, or zero when
// RankedOptions.ExactK leaves every selected candidate sized.
func (w *Worker) exactLimit(engine *Engine) int {
	if w.opts.ExactK > 0 && w.opts.ExactK < engine.K() {
		return w.opts.ExactK
	}
	return 0
}

// exactBudget grants full amount searches to one funding asset's candidates
// in the order the streamed ranking admits them: short-route batches first,
// rank order within each batch. A later candidate whose coarse estimate beats
// every granted one may still use a reserve of limit/4, mirroring the probe
// admission reserve, so an early small route cannot hold every slot.
type exactBudget struct {
	granted int
	best    *big.Int
}

func grantSizing(budgets map[domain.Address]*exactBudget, candidate rankedCycle, limit int) bool {
	if limit == 0 {
		return true
	}
	budget := budgets[candidate.cycle.Start]
	if budget == nil {
		budget = &exactBudget{}
		budgets[candidate.cycle.Start] = budget
	}
	leader := budget.best == nil || candidate.net.Cmp(budget.best) > 0
	if budget.granted >= limit && (!leader || budget.granted >= limit+max(1, limit/4)) {
		return false
	}
	budget.granted++
	if leader {
		budget.best = candidate.net
	}
	return true
}

// limitPerAsset keeps the first limit candidates of each funding asset. The
// selection is grouped by asset and rank-ordered within it, so these are the
// top-ranked ones. Unstreamed searches publish nothing before sizing, so the
// rest are dropped rather than left at an initial quote.
func (s RankedSelection) limitPerAsset(limit int) RankedSelection {
	if limit == 0 {
		return s
	}
	kept := make([]rankedCycle, 0, len(s.candidates))
	run := 0
	for index, candidate := range s.candidates {
		if index == 0 || candidate.cycle.Start != s.candidates[index-1].cycle.Start {
			run = 0
		}
		if run < limit {
			kept = append(kept, candidate)
		}
		run++
	}
	s.candidates = kept
	return s
}
