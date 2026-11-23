package planning

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

// QuoteTransactionBound covers any effective priority fee up to the signed cap:
// the MEV tax only grows with the tip and is largest at a zero base fee. Our
// sender has no account discount, so the hook's tick-spacing fallback, which
// needs a fully discounted zero fee, does not apply; charging it anyway cost up
// to 1% per dynamic-fee pool and rejected every such route (159027597).
func QuoteTransactionBound(ctx context.Context, view arb.View, o arb.Opportunity, priorityCap *big.Int) (arb.Opportunity, []execution.ReactivePoolFee, error) {
	return QuoteTransaction(ctx, view, o, new(big.Int), priorityCap, 0)
}

// QuoteTransaction recomputes every leg after resolving transaction-sensitive
// pool fees for a specified base fee, effective tip and resolved discount.
// Search keeps its shared zero-tip caches; only shortlisted routes pay this cost.
func QuoteTransaction(ctx context.Context, view arb.View, o arb.Opportunity, baseFee, priorityFee *big.Int, discount uint32) (arb.Opportunity, []execution.ReactivePoolFee, error) {
	if o.Input == nil || o.Input.Sign() <= 0 || baseFee == nil || priorityFee == nil || baseFee.Sign() < 0 || priorityFee.Sign() < 0 {
		return arb.Opportunity{}, nil, fmt.Errorf("invalid transaction quote context")
	}
	token, amount := o.Cycle.Start, new(big.Int).Set(o.Input)
	quoted := arb.Opportunity{Cycle: o.Cycle, Input: new(big.Int).Set(o.Input)}
	var fees []execution.ReactivePoolFee
	seen := make(map[domain.PoolKey]bool, len(o.Cycle.Pools))
	for _, key := range o.Cycle.Pools {
		if seen[key] {
			return arb.Opportunity{}, nil, fmt.Errorf("transaction quote repeats a pool")
		}
		seen[key] = true
		p, ok := view.Pool(key)
		if !ok {
			return arb.Opportunity{}, nil, arb.ErrMissingPool
		}
		if token != p.Token0 && token != p.Token1 {
			return arb.Opportunity{}, nil, pool.ErrInvalidState
		}
		adapter, err := dex.NewAdapter(p)
		if err != nil {
			return arb.Opportunity{}, nil, err
		}
		if key.Kind == domain.PoolKindSlipstream {
			fee, err := slipstream.TransactionFee(p, baseFee, priorityFee, discount)
			if err != nil {
				return arb.Opportunity{}, nil, err
			}
			fees = append(fees, execution.ReactivePoolFee{Pool: key, FeePips: fee, BaseFee: new(big.Int).Set(baseFee), PriorityFee: new(big.Int).Set(priorityFee), Discount: discount})
			// The resolved fee belongs only to this quote; never mutate shared oracle
			// state or the adapter caches used by discovery.
			adapter, err = uniswapv3.New(fee, p.TickSpacing)
			if err != nil {
				return arb.Opportunity{}, nil, err
			}
			p.Key.Kind = domain.PoolKindV3
		}
		deep := pool.DeepState{}
		if key.Kind.UsesTicks() {
			deep, ok = view.Deep(key)
			if !ok {
				return arb.Opportunity{}, nil, arb.ErrMissingDeep
			}
		}
		output, err := adapter.QuoteExactIn(ctx, p, deep, token, amount)
		if err != nil {
			return arb.Opportunity{}, nil, err
		}
		quoted.Legs = append(quoted.Legs, arb.Leg{Pool: key, AmountIn: amount, AmountOut: output})
		if token == p.Token0 {
			token = p.Token1
		} else {
			token = p.Token0
		}
		amount = output
	}
	if token != o.Cycle.Start {
		return arb.Opportunity{}, nil, fmt.Errorf("transaction quote does not close")
	}
	quoted.Output, quoted.Net = amount, new(big.Int).Sub(amount, o.Input)
	return quoted, fees, nil
}

// HasTransactionFees reports whether a route needs transaction-context fee math.
func HasTransactionFees(o arb.Opportunity) bool {
	for _, key := range o.Cycle.Pools {
		if key.Kind == domain.PoolKindSlipstream {
			return true
		}
	}
	return false
}
