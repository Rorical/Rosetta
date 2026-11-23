package arb

import (
	"math"
	"math/big"

	"github.com/Rorical/Rosetta/internal/graph"
)

func (w *rankWorkspace) genericUpper(snapshot *rankSnapshot, cycle graph.Cycle, minimumLow, maximumHigh float64) float64 {
	token := cycle.Start
	for index, key := range cycle.Pools {
		leg, next, err := snapshot.leg(key, token)
		if err != nil {
			return math.Inf(1)
		}
		w.intervals[index] = leg.interval
		token = next
	}
	return modelCycleUpper(w.intervals, 0, minimumLow, maximumHigh)
}

func (b *rankBucket) rejectPathUpper(upper *big.Int, cycle graph.Cycle, limit int) bool {
	if len(b.profit) < limit || upper.Cmp(b.profit[0].net) >= 0 {
		return false
	}
	if !b.diverse {
		return true
	}
	depth := b.depth[len(cycle.Pools)]
	if len(depth) < depthReservoirLimit(limit) || upper.Cmp(depth[0].net) >= 0 {
		return false
	}
	for _, key := range cycle.Pools {
		prior, ok := b.pools[key]
		if !ok || upper.Cmp(prior.net) >= 0 {
			return false
		}
	}
	return true
}
