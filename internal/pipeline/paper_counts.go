package pipeline

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"sync/atomic"
)

type economicsCounters [2][4]atomic.Uint64
type economicsCounts [2][4]uint64

var economicsStatuses = [4]EconomicsStatus{NetUnknown, NetNonPositive, NetBelowMinimum, NetEligible}

func (c *economicsCounters) record(stage arb.QuoteStage, status EconomicsStatus) {
	row := 0
	if stage == arb.QuoteOptimized {
		row = 1
	}
	for i, s := range economicsStatuses {
		if s == status {
			c[row][i].Add(1)
			return
		}
	}
	c[row][0].Add(1)
}
func (c *economicsCounters) snapshot() (out economicsCounts) {
	for row := range c {
		for i := range c[row] {
			out[row][i] = c[row][i].Load()
		}
	}
	return
}
func (c economicsCounts) report() map[string]map[EconomicsStatus]uint64 {
	out := map[string]map[EconomicsStatus]uint64{}
	for row, name := range []string{"initial", "optimized"} {
		out[name] = map[EconomicsStatus]uint64{}
		for i, status := range economicsStatuses {
			out[name][status] = c[row][i]
		}
	}
	return out
}
