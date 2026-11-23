package remote

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/planning"
)

// flashSwapTurn turns o's loop to start on its first pool that lends inside
// its own swap and is entered with a configured asset, and returns the amount
// that leg took in as the new principal. The search reports a loop from one
// start only; when that start's pool cannot lend, the route borrows from a
// standalone flash loan, which costs about 190,000 gas more than the same
// loop started on a lending pool.
func flashSwapTurn(view arb.View, o arb.Opportunity, configured func(domain.Address) bool) (graph.Cycle, *big.Int, bool) {
	pools := o.Cycle.Pools
	if len(pools) < 2 || len(o.Legs) != len(pools) || planning.LendsInSwap(pools[0].Kind) {
		return graph.Cycle{}, nil, false
	}
	current := o.Cycle.Start
	for offset, key := range pools {
		if offset > 0 && planning.LendsInSwap(key.Kind) && configured(current) && o.Legs[offset].AmountIn != nil {
			turned := make([]domain.PoolKey, 0, len(pools))
			turned = append(append(turned, pools[offset:]...), pools[:offset]...)
			return graph.Cycle{Start: current, Pools: turned, Len: len(turned)}, o.Legs[offset].AmountIn, true
		}
		p, ok := view.Pool(key)
		if !ok {
			return graph.Cycle{}, nil, false
		}
		switch current {
		case p.Token0:
			current = p.Token1
		case p.Token1:
			current = p.Token0
		default:
			return graph.Cycle{}, nil, false
		}
	}
	return graph.Cycle{}, nil, false
}
