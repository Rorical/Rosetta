package solidlyv3

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// ErrInvalidResponse reports a slot0 or SetFee payload that is malformed or out
// of range.
var ErrInvalidResponse = errors.New("solidlyv3: invalid response")

// maxFeePips is the pool's setFee cap: 10% in hundredths of a basis point.
const maxFeePips uint32 = 100_000

// Adapter prices one Solidly V3 pool with the fee recorded in its state.
type Adapter struct {
	tickSpacing int32
}

var _ pool.Adapter = (*Adapter)(nil)

// New returns an adapter for a pool with the given tick spacing. The fee is
// read from each state because SetFee can change it between quotes.
func New(tickSpacing int32) (*Adapter, error) {
	if tickSpacing <= 0 {
		return nil, fmt.Errorf("%w: tick spacing %d must be positive", uniswapv3.ErrInvalidConfig, tickSpacing)
	}
	return &Adapter{tickSpacing: tickSpacing}, nil
}

// Kind reports the pool family this adapter prices.
func (a *Adapter) Kind() domain.PoolKind {
	return domain.PoolKindSolidlyV3
}

// QuoteExactIn replays the V3 swap against state and deep without modifying
// either.
func (a *Adapter) QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Key.Kind != domain.PoolKindSolidlyV3 {
		return nil, fmt.Errorf("%w: pool kind %d is not Solidly V3", pool.ErrInvalidState, state.Key.Kind)
	}
	if state.Fee > maxFeePips {
		return nil, fmt.Errorf("%w: fee %d pips above the pool cap", pool.ErrInvalidState, state.Fee)
	}
	inner, err := uniswapv3.New(state.Fee, a.tickSpacing)
	if err != nil {
		return nil, err
	}
	v3State := state
	v3State.Key.Kind = domain.PoolKindV3
	return inner.QuoteExactIn(ctx, v3State, deep, tokenIn, amountIn)
}

// ExactInputMonotone certifies fixed-state quotes: the math is Uniswap V3's.
func (a *Adapter) ExactInputMonotone(state pool.State) bool { return true }
