package state

import (
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/pool"
)

// A complete bitmap window can consume liquidity deltas without RPC. Missing
// burn boundaries inside that window indicate incomplete input; boundaries
// outside it affect active liquidity but do not change its initialized ticks.
func preservesTickWindow(deep pool.DeepState, event uniswapv3.Event) bool {
	if event.Kind == uniswapv3.EventInitialize || deep.WindowMin == 0 && deep.WindowMax == 0 {
		return false
	}
	if event.Kind != uniswapv3.EventBurn {
		return true
	}
	for _, tick := range []int32{event.TickLower, event.TickUpper} {
		if tick < deep.WindowMin || tick > deep.WindowMax {
			continue
		}
		if _, ok := deep.Ticks[tick]; !ok {
			return false
		}
	}
	return true
}
