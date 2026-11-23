package planning

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

var maxPriceBound = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))

// SetPriceBounds gives each step a one-sided pre-swap price bound, which the
// executor checks before funding or the first swap (packed version 3). A bound
// is the state at which that leg's spot rate has fallen by the route's slack,
// 1 − required/output: with every other pool unchanged, the route still
// returns its principal, minimum profit and loan-fee cap. output is the quoted
// route output and view the state it was quoted on.
//
// A rival that takes the opportunity first moves the pools it shares with us
// by about the whole margin, at least twice the slack of an optimally sized
// route whose minimum is any part of its profit, and mostly on the pool whose
// mispricing it corrects. Giving each pool the whole slack rejects a route
// that would still succeed only when another pool moved in its favour; a rival
// move spread so thinly that no pool passes the slack still fails at the
// closing profit check, as without bounds.
func SetPriceBounds(view arb.View, plan *execution.ExecutorPlan, output *big.Int) error {
	required := new(big.Int).Add(plan.Amount, plan.MinProfit)
	required.Add(required, plan.MaxLoanFee)
	if output.Cmp(required) <= 0 {
		return fmt.Errorf("quoted output leaves no slack for price bounds")
	}
	for i := range plan.Steps {
		step := &plan.Steps[i]
		state, ok := view.Pool(step.Pool)
		if !ok {
			return fmt.Errorf("step %d: pool missing from the quote view", i)
		}
		bound, err := priceBound(state, bytes.Compare(step.TokenIn[:], step.TokenOut[:]) < 0, required, output)
		if err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		step.PriceBound = bound
	}
	return nil
}

// priceBound rounds toward acceptance. Nil means the step is not checked.
func priceBound(state pool.State, zeroForOne bool, required, output *big.Int) (*big.Int, error) {
	var bound *big.Int
	switch state.Key.Kind {
	case domain.PoolKindV2, domain.PoolKindVelodromeV2:
		// A stable curve's reserve ratio moves far more than its rate, so a
		// ratio bound would reject trades that barely change our output.
		if state.Velodrome != nil && state.Velodrome.Stable {
			return nil, nil
		}
		if state.Reserve0 == nil || state.Reserve1 == nil || state.Reserve0.Sign() <= 0 || state.Reserve1.Sign() <= 0 {
			return nil, fmt.Errorf("pair reserves unavailable for a price bound")
		}
		in, out := state.Reserve0, state.Reserve1
		if !zeroForOne {
			in, out = out, in
		}
		// reserveOut/reserveIn in Q96, as the executor compares it.
		bound = new(big.Int).Lsh(out, 96)
		bound.Quo(bound, in).Mul(bound, required).Quo(bound, output)
	case domain.PoolKindV3, domain.PoolKindCamelotV3, domain.PoolKindSlipstream, domain.PoolKindSolidlyV3, domain.PoolKindV4:
		if state.SqrtPriceX96 == nil || state.SqrtPriceX96.Sign() <= 0 {
			return nil, fmt.Errorf("sqrt price unavailable for a price bound")
		}
		squared := new(big.Int).Mul(state.SqrtPriceX96, state.SqrtPriceX96)
		if zeroForOne {
			// The leg's rate is price²: the lowest price keeping required/output of it.
			bound = squared.Mul(squared, required).Quo(squared, output).Sqrt(squared)
		} else {
			// The rate is 1/price²: the highest price, rounded up.
			squared.Mul(squared, output)
			squared.Add(squared, new(big.Int).Sub(required, big.NewInt(1))).Quo(squared, required)
			bound = new(big.Int).Sqrt(squared)
			if new(big.Int).Mul(bound, bound).Cmp(squared) < 0 {
				bound.Add(bound, big.NewInt(1))
			}
		}
	case domain.PoolKindMetricOmm:
		// The executor's check reads no MetricOmm pool, and its price is an
		// oracle quote that moves every two seconds without a trade.
		return nil, nil
	case domain.PoolKindWrap:
		// A deposit or withdrawal is 1:1: there is no price to move.
		return nil, nil
	default:
		return nil, fmt.Errorf("no price bound for pool kind %d", state.Key.Kind)
	}
	if bound.Sign() == 0 {
		return nil, nil
	}
	if bound.Cmp(maxPriceBound) > 0 {
		bound.Set(maxPriceBound)
	}
	return bound, nil
}

// SetPriceReferences prepares the cumulative precheck (packed version 4): each
// step's price at the quote and MinRate, the least product of the legs' rate
// changes that still returns required out of output. Several pools that each
// moved less than the slack then fail together before swapping, where per-step
// bounds let them reach the closing profit check. Prices
// round as the executor computes them, so an unchanged pool is exactly 1.0;
// MinRate rounds down, toward acceptance.
func SetPriceReferences(view arb.View, plan *execution.ExecutorPlan, output *big.Int) error {
	required := new(big.Int).Add(plan.Amount, plan.MinProfit)
	required.Add(required, plan.MaxLoanFee)
	if output.Cmp(required) <= 0 {
		return fmt.Errorf("quoted output leaves no slack for price bounds")
	}
	for i := range plan.Steps {
		step := &plan.Steps[i]
		state, ok := view.Pool(step.Pool)
		if !ok {
			return fmt.Errorf("step %d: pool missing from the quote view", i)
		}
		reference, err := priceReference(state, bytes.Compare(step.TokenIn[:], step.TokenOut[:]) < 0)
		if err != nil {
			return fmt.Errorf("step %d: %w", i, err)
		}
		step.PriceBound = reference
	}
	rate := new(big.Int).Mul(required, big.NewInt(1e18))
	plan.MinRate = rate.Quo(rate, output)
	if plan.MinRate.Sign() == 0 {
		return fmt.Errorf("quoted output leaves no rate for the precheck")
	}
	return nil
}

// priceReference is the leg's price as the executor reads it. Nil means the
// step is not checked.
func priceReference(state pool.State, zeroForOne bool) (*big.Int, error) {
	var reference *big.Int
	switch state.Key.Kind {
	case domain.PoolKindV2, domain.PoolKindVelodromeV2:
		// As in priceBound: a stable curve's reserve ratio overstates its rate change.
		if state.Velodrome != nil && state.Velodrome.Stable {
			return nil, nil
		}
		if state.Reserve0 == nil || state.Reserve1 == nil || state.Reserve0.Sign() <= 0 || state.Reserve1.Sign() <= 0 {
			return nil, fmt.Errorf("pair reserves unavailable for a price bound")
		}
		in, out := state.Reserve0, state.Reserve1
		if !zeroForOne {
			in, out = out, in
		}
		reference = new(big.Int).Lsh(out, 96)
		reference.Quo(reference, in)
	case domain.PoolKindV3, domain.PoolKindCamelotV3, domain.PoolKindSlipstream, domain.PoolKindSolidlyV3, domain.PoolKindV4:
		if state.SqrtPriceX96 == nil || state.SqrtPriceX96.Sign() <= 0 {
			return nil, fmt.Errorf("sqrt price unavailable for a price bound")
		}
		reference = new(big.Int).Set(state.SqrtPriceX96)
	case domain.PoolKindMetricOmm:
		// The executor's check reads no MetricOmm pool, and its price is an
		// oracle quote that moves every two seconds without a trade.
		return nil, nil
	case domain.PoolKindWrap:
		// A deposit or withdrawal is 1:1: there is no price to move.
		return nil, nil
	default:
		return nil, fmt.Errorf("no price bound for pool kind %d", state.Key.Kind)
	}
	// A price beyond the 20-byte field cannot be encoded; leave the leg unchecked.
	if reference.Sign() == 0 || reference.Cmp(maxPriceBound) > 0 {
		return nil, nil
	}
	return reference, nil
}
