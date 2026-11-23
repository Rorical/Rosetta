package uniswapv2

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// maxFeePips is 100% in hundredths of a basis point, the V2 fee unit.
const maxFeePips uint32 = 1_000_000

// Adapter prices one constant-product pool.
type Adapter struct {
	feePips uint32
}

var _ pool.Adapter = (*Adapter)(nil)

// New returns an adapter for a pool with the given fee.
func New(feePips uint32) (*Adapter, error) {
	if feePips >= maxFeePips {
		return nil, fmt.Errorf("%w: fee %d pips outside [0, %d)", ErrInvalidConfig, feePips, maxFeePips)
	}
	return &Adapter{feePips: feePips}, nil
}

// Kind reports the pool family this adapter prices.
func (a *Adapter) Kind() domain.PoolKind {
	return domain.PoolKindV2
}

// QuoteExactIn returns the token output for amountIn of tokenIn. V2 pricing
// depends only on shallow reserves; deep state is ignored.
func (a *Adapter) QuoteExactIn(ctx context.Context, state pool.State, _ pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Key.Kind != domain.PoolKindV2 {
		return nil, fmt.Errorf("%w: pool kind %d is not V2", pool.ErrInvalidState, state.Key.Kind)
	}

	var reserveIn, reserveOut *big.Int
	switch tokenIn {
	case state.Token0:
		reserveIn, reserveOut = state.Reserve0, state.Reserve1
	case state.Token1:
		reserveIn, reserveOut = state.Reserve1, state.Reserve0
	default:
		return nil, fmt.Errorf("%w: token %s", ErrTokenNotInPool, tokenIn)
	}
	if reserveIn == nil || reserveOut == nil {
		return nil, fmt.Errorf("%w: nil reserve", pool.ErrInvalidState)
	}
	if reserveIn.Sign() <= 0 || reserveOut.Sign() <= 0 {
		return nil, fmt.Errorf("%w: reserves %s/%s", pool.ErrNoLiquidity, reserveIn, reserveOut)
	}

	out, err := math.QuoteV2ExactIn(reserveIn, reserveOut, a.feePips, amountIn)
	if err != nil {
		return nil, fmt.Errorf("uniswapv2: quote: %w", err)
	}
	return out, nil
}

// ExactInputMonotone certifies the fixed-fee curve used by this adapter.
func (a *Adapter) ExactInputMonotone(state pool.State) bool { return true }
