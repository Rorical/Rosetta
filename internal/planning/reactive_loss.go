package planning

import (
	"github.com/Rorical/Rosetta/internal/arb"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// FreeLoss is the loss gas of a hot route whose every pool a condition
// holds: a rival that takes the opportunity first moves one of them, and the
// sequencer drops our transaction without charge. The tip model
// reads zero as unknown, so free is one gas.
const FreeLoss = 1

// hotBoundGas is executeV3Hot's gas inside the call up to a failing price
// bound: 12,902 (two hops) and 13,267 (three hops) on the Arbitrum One fork
// at block 522800617.
const hotBoundGas = 13_500

// lossGas is what a lost race costs a route that goes out conditionally,
// before the estimator learns from outcomes, and whether it does. Any route
// whose every pool a condition holds loses free. A hot route with an unheld
// pool fails at that pool's bound and pays the intrinsic, calldata and poster
// gas besides; a packed one keeps its precheck's prior. Zero leaves the
// estimator's priors.
func lossGas(cfg ReactivePolicy, view arb.View, o arb.Opportunity, funding execution.Funding, costs ReactiveCosts) (uint64, bool) {
	supported, bound := execution.HotPoolsSupported, execution.HotCalldataBound
	if cfg.HotChain {
		supported, bound = execution.HotChainPoolsSupported, execution.HotChainCalldataBound
	}
	hot := cfg.HotPath && costs.ConfigVersion != 0 && supported(funding, o.Cycle.Pools)
	if !hot && !cfg.PackedConditions {
		return 0, false
	}
	held := 0
	if cfg.QuietPool != nil {
		for _, key := range o.Cycle.Pools {
			if stateless(key) {
				held++
				continue
			}
			state, ok := view.Pool(key)
			if !ok {
				continue
			}
			// A MetricOmm leg is held at its position only (holdQuietPools):
			// a newer oracle quote ahead of us is paid for, so it is unheld.
			if len(cfg.QuietPool(state)) > 0 {
				held++
			}
		}
	}
	switch {
	case held == len(o.Cycle.Pools):
		return FreeLoss, true
	case hot:
		size := bound(len(o.Cycle.Pools))
		return 21_000 + 16*uint64(size) + hotBoundGas + costs.PosterGasAt(size), true
	case held > 0:
		return 0, true
	}
	return 0, false
}
