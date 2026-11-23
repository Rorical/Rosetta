package arb

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

var (
	ErrMissingPool = errors.New("arb: pool state missing")
	ErrMissingDeep = errors.New("arb: tick state missing or stale")
)

// QuoteIssue preserves why a route could not be priced. In particular, missing
// ticks or a quote outside the loaded window require a refresh, not zero profit.
type QuoteIssue struct {
	Pool domain.PoolKey
	Err  error
}

func (e *QuoteIssue) Error() string { return fmt.Sprintf("quote pool %s: %v", e.Pool.Address, e.Err) }
func (e *QuoteIssue) Unwrap() error { return e.Err }

// quote retains the legacy evaluator's omission behavior. Live search uses
// quoteExact so it can report unavailable pool dependencies to its caller.
func (e *Engine) quote(ctx context.Context, view View, cycle graph.Cycle, input *big.Int) (Opportunity, bool, error) {
	opportunity, err := e.quoteExact(ctx, view, cycle, input)
	if err != nil {
		if ctx.Err() != nil {
			return Opportunity{}, false, ctx.Err()
		}
		return Opportunity{}, false, nil
	}
	return opportunity, true, nil
}

func (e *Engine) quoteExact(ctx context.Context, view View, cycle graph.Cycle, input *big.Int) (Opportunity, error) {
	return e.quoteInto(ctx, view, cycle, input, make([]Leg, 0, len(cycle.Pools)), new(big.Int))
}

// quoteInto borrows legs and net only for sizing; public quotes own both.
func (e *Engine) quoteInto(ctx context.Context, view View, cycle graph.Cycle, input *big.Int, legs []Leg, net *big.Int) (Opportunity, error) {
	amountIn := input
	current := cycle.Start
	for _, key := range cycle.Pools {
		if err := ctx.Err(); err != nil {
			return Opportunity{}, err
		}
		state, ok := view.Pool(key)
		if !ok {
			return Opportunity{}, &QuoteIssue{Pool: key, Err: ErrMissingPool}
		}
		tokenOut, ok := otherToken(state, current)
		if !ok {
			return Opportunity{}, &QuoteIssue{Pool: key, Err: pool.ErrInvalidState}
		}
		deep := pool.DeepState{}
		if needsDeep(state.Key.Kind) {
			deep, ok = view.Deep(key)
			if !ok {
				return Opportunity{}, &QuoteIssue{Pool: key, Err: ErrMissingDeep}
			}
		}
		var amountOut *big.Int
		var err error
		if frozen, ok := view.(*quoteView); ok && frozen.memo != nil {
			amountOut, err = frozen.memo.quote(ctx, e.quoter, state, deep, current, amountIn)
		} else {
			amountOut, err = e.quoter.QuoteExactIn(ctx, state, deep, current, amountIn)
		}
		if err != nil {
			return Opportunity{}, &QuoteIssue{Pool: key, Err: err}
		}
		legs = append(legs, Leg{Pool: key, AmountIn: amountIn, AmountOut: amountOut})
		if amountOut.Sign() == 0 {
			// Integer rounding exhausted the input. No downstream state can
			// make this input profitable; requesting its ticks wastes reads.
			return Opportunity{Cycle: cycle, Input: input, Output: amountOut, Net: net.Neg(input), Legs: legs}, nil
		}
		current, amountIn = tokenOut, amountOut
	}
	if current != cycle.Start {
		return Opportunity{}, fmt.Errorf("%w: route does not close", ErrInvalidConfig)
	}
	return Opportunity{Cycle: cycle, Input: input, Output: amountIn, Net: net.Sub(amountIn, input), Legs: legs}, nil
}

// QuoteCycle prices one fixed-input route without ranking or amount optimization.
// It is used by observation and historical diagnostics; all reads stay in view.
func (e *Engine) QuoteCycle(ctx context.Context, view View, cycle graph.Cycle, input *big.Int) (Opportunity, error) {
	if input == nil || input.Sign() <= 0 || input.BitLen() > 256 {
		return Opportunity{}, fmt.Errorf("invalid route input")
	}
	return e.quoteExact(ctx, view, cycle, new(big.Int).Set(input))
}
