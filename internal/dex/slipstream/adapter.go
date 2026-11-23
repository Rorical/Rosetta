package slipstream

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Adapter prices one Slipstream pool using a snapshot fee in V3 pips (1e-6).
// The fee module can depend on price, time and sender; a cached fee is not a
// promise about the fee at a later execution position. Tick spacing is fixed.
type Adapter struct {
	inner uniswapv3.Adapter
}

var _ pool.Adapter = (*Adapter)(nil)

// New returns an adapter for a pool with the given fee and tick spacing.
func New(feePips uint32, tickSpacing int32) (*Adapter, error) {
	inner, err := uniswapv3.New(feePips, tickSpacing)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidConfig, err)
	}
	return &Adapter{inner: *inner}, nil
}

// Kind reports the pool family this adapter prices.
func (a *Adapter) Kind() domain.PoolKind {
	return domain.PoolKindSlipstream
}

// QuoteExactIn returns the token output for amountIn of tokenIn. It replays the
// swap against state and deep without modifying either.
func (a *Adapter) QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Key.Kind != domain.PoolKindSlipstream {
		return nil, fmt.Errorf("%w: pool kind %d is not Slipstream", pool.ErrInvalidState, state.Key.Kind)
	}
	// The inner adapter prices the V3 state shape and rejects other routing
	// labels; the kind is a label, not part of the quote input.
	v3State := state
	v3State.Key.Kind = domain.PoolKindV3
	fee, err := LocalFee(state)
	if err != nil {
		return nil, err
	}
	inner := &a.inner
	if state.Slipstream != nil {
		inner, err = uniswapv3.New(fee, state.TickSpacing)
		if err != nil {
			return nil, err
		}
	}
	amountOut, err := inner.QuoteExactIn(ctx, v3State, deep, tokenIn, amountIn)
	if err != nil {
		return nil, fmt.Errorf("slipstream: quote: %w", err)
	}
	return amountOut, nil
}

// ExactInputMonotone certifies the fixed-fee curve used by this adapter.
func (a *Adapter) ExactInputMonotone(state pool.State) bool { return true }
