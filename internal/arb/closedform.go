package arb

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// closedFormLegs derives both legs of a two-pool cycle in traversal order. It
// reports false for longer cycles and whenever a leg's reserves cannot be read
// from shallow state; callers then keep the fixed-notional quote.
//
// The fee comes from pool.State.Fee because the Quoter interface carries no fee
// and adapters are configured from the same state. V3 reserves are virtual
// reserves at the current price, so the result is exact only while the swap
// stays inside the current tick range. The exact quote decides whether the
// candidate is actually better. V4 is excluded: its effective fee is
// directional and computed by the adapter, so State.Fee would mis-size the leg;
// V4 cycles keep the fixed-notional quote.
func closedFormLegs(view View, cycle graph.Cycle) (math.ClosedFormLeg, math.ClosedFormLeg, bool) {
	if len(cycle.Pools) != 2 {
		return math.ClosedFormLeg{}, math.ClosedFormLeg{}, false
	}

	var legs [2]math.ClosedFormLeg
	current := cycle.Start
	for i, key := range cycle.Pools {
		state, ok := view.Pool(key)
		if !ok {
			return math.ClosedFormLeg{}, math.ClosedFormLeg{}, false
		}
		inReserve, outReserve, ok := traversalReserves(state, current)
		if !ok {
			return math.ClosedFormLeg{}, math.ClosedFormLeg{}, false
		}
		tokenOut, ok := otherToken(state, current)
		if !ok {
			return math.ClosedFormLeg{}, math.ClosedFormLeg{}, false
		}
		legs[i] = math.ClosedFormLeg{
			InReserve:  inReserve,
			OutReserve: outReserve,
			FeePips:    state.Fee,
		}
		current = tokenOut
	}
	return legs[0], legs[1], true
}

// traversalReserves returns the reserves of tokenIn and its counterpart for one
// leg. It reports false for unsupported pool kinds and unusable state.
func traversalReserves(state pool.State, tokenIn domain.Address) (*big.Int, *big.Int, bool) {
	switch state.Key.Kind {
	case domain.PoolKindV2:
		switch tokenIn {
		case state.Token0:
			return positiveReserves(state.Reserve0, state.Reserve1)
		case state.Token1:
			return positiveReserves(state.Reserve1, state.Reserve0)
		}
	case domain.PoolKindV3:
		token0, token1, ok := virtualReserves(state)
		if !ok {
			return nil, nil, false
		}
		switch tokenIn {
		case state.Token0:
			return positiveReserves(token0, token1)
		case state.Token1:
			return positiveReserves(token1, token0)
		}
	}
	return nil, nil, false
}

// virtualReserves returns the token0 and token1 reserves implied by in-range
// liquidity at the current price: x = L*Q96/sqrtP and y = L*sqrtP/Q96.
func virtualReserves(state pool.State) (*big.Int, *big.Int, bool) {
	if state.Liquidity == nil || state.SqrtPriceX96 == nil {
		return nil, nil, false
	}
	if state.Liquidity.Sign() <= 0 || state.SqrtPriceX96.Sign() <= 0 {
		return nil, nil, false
	}
	token0 := new(big.Int).Div(new(big.Int).Mul(state.Liquidity, math.Q96), state.SqrtPriceX96)
	token1 := new(big.Int).Div(new(big.Int).Mul(state.Liquidity, state.SqrtPriceX96), math.Q96)
	return token0, token1, true
}

// positiveReserves returns in and out when both are positive. Flooring a
// virtual reserve can reach zero near the tick bounds, which makes the leg
// unusable for the closed form.
func positiveReserves(in, out *big.Int) (*big.Int, *big.Int, bool) {
	if in == nil || out == nil || in.Sign() <= 0 || out.Sign() <= 0 {
		return nil, nil, false
	}
	return in, out, true
}
