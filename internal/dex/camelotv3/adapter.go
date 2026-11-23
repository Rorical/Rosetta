package camelotv3

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// maxFeePips is 100% in hundredths of a basis point, the Algebra fee unit.
const maxFeePips uint32 = 1_000_000

// Adapter prices one Camelot V3 pool. FeePips and TickSpacing are fixed when
// the adapter is built; rebuild it when the pool's on-chain fee changes.
type Adapter struct {
	FeePips     uint32
	TickSpacing int32
}

var _ pool.Adapter = (*Adapter)(nil)

// New returns an adapter for a pool with the given fee and tick spacing.
// feePips is in hundredths of a basis point, the Algebra unit.
func New(feePips uint32, tickSpacing int32) (*Adapter, error) {
	if feePips >= maxFeePips {
		return nil, fmt.Errorf("%w: fee %d pips outside [0, %d)", ErrInvalidConfig, feePips, maxFeePips)
	}
	if tickSpacing <= 0 {
		return nil, fmt.Errorf("%w: tick spacing %d must be positive", ErrInvalidConfig, tickSpacing)
	}
	return &Adapter{FeePips: feePips, TickSpacing: tickSpacing}, nil
}

// Kind reports the pool family this adapter prices.
func (a *Adapter) Kind() domain.PoolKind {
	return domain.PoolKindCamelotV3
}

// QuoteExactIn returns the token output for amountIn of tokenIn. It replays the
// swap against state and deep without modifying either.
func (a *Adapter) QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Key.Kind != domain.PoolKindCamelotV3 {
		return nil, fmt.Errorf("%w: pool kind %d is not Camelot V3", pool.ErrInvalidState, state.Key.Kind)
	}

	var zeroForOne bool
	switch tokenIn {
	case state.Token0:
		zeroForOne = true
	case state.Token1:
		zeroForOne = false
	default:
		return nil, fmt.Errorf("%w: token %s", ErrTokenNotInPool, tokenIn)
	}

	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount in must be positive", math.ErrInvalidArgument)
	}
	if state.Liquidity == nil {
		return nil, fmt.Errorf("%w: nil liquidity", pool.ErrInvalidState)
	}
	if state.Liquidity.Sign() == 0 {
		return nil, fmt.Errorf("%w: pool %s", pool.ErrNoLiquidity, state.Key.Address)
	}
	if state.Liquidity.Sign() < 0 {
		return nil, fmt.Errorf("%w: negative liquidity", pool.ErrInvalidState)
	}

	fee := a.FeePips
	if !zeroForOne && state.FeeOneForZero != nil {
		fee = *state.FeeOneForZero
	}
	amountOut, err := math.QuoteV3ExactIn(math.V3State{
		SqrtPriceX96: state.SqrtPriceX96,
		Tick:         state.Tick,
		Liquidity:    state.Liquidity,
		FeePips:      fee,
		TickSpacing:  a.TickSpacing,
		Bitmap:       deep.Bitmap,
		Ticks:        deep.Ticks,
		WindowMin:    deep.WindowMin,
		WindowMax:    deep.WindowMax,
	}, zeroForOne, amountIn)
	if err != nil {
		return nil, fmt.Errorf("camelotv3: quote: %w", err)
	}
	return amountOut, nil
}

// ExactInputMonotone certifies the fixed-fee curve used by this adapter.
func (a *Adapter) ExactInputMonotone(state pool.State) bool { return true }
