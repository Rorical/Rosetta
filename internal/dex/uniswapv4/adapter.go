package uniswapv4

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Adapter prices one hooks-free V4 pool. The LP fee and the directional protocol
// fee travel in pool.State, so each quote uses what the last load or log apply
// stored.
//
// Known staleness: ProtocolFee is read from slot0 at load time and never
// updated from logs. Swap events carry the fee actually charged, but this
// package ignores that word, so a ProtocolFeeController change mis-prices the
// pool until slot0 is read again. The bot's refreshFees does that for every V4
// pool before each paper quote; a direct adapter caller must arrange its own
// refresh across a fee change.
type Adapter struct{}

var _ pool.Adapter = (*Adapter)(nil)

// New returns a V4 quote adapter. Pool parameters travel with each quote, so
// there is nothing to configure here.
func New() (*Adapter, error) {
	return &Adapter{}, nil
}

// Kind reports the pool family this adapter prices.
func (a *Adapter) Kind() domain.PoolKind {
	return domain.PoolKindV4
}

// QuoteExactIn returns the token output for amountIn of tokenIn. It replays the
// swap against state and deep without modifying either.
//
// Pools with hooks are rejected: a hook can override the swap fee or change the
// swap path, so the V3 curve is not a valid model for them.
func (a *Adapter) QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Key.Kind != domain.PoolKindV4 {
		return nil, fmt.Errorf("%w: pool kind %d is not V4", pool.ErrInvalidState, state.Key.Kind)
	}
	if state.Key.Hooks != (domain.Address{}) {
		return nil, fmt.Errorf("%w: hooks %s", ErrUnsupportedHooks, state.Key.Hooks)
	}
	if state.Key.PoolID == (domain.Hash{}) {
		return nil, fmt.Errorf("%w: zero pool ID", pool.ErrInvalidState)
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

	if state.Liquidity == nil {
		return nil, fmt.Errorf("%w: nil liquidity", pool.ErrInvalidState)
	}
	if state.Liquidity.Sign() == 0 {
		return nil, fmt.Errorf("%w: pool %s", pool.ErrNoLiquidity, state.Key.PoolID.Hex())
	}
	if state.Liquidity.Sign() < 0 {
		return nil, fmt.Errorf("%w: negative liquidity", pool.ErrInvalidState)
	}
	if deep.Bitmap == nil || deep.Ticks == nil {
		return nil, fmt.Errorf("%w: pool %s", ErrMissingTickState, state.Key.PoolID.Hex())
	}

	feePips, err := SwapFee(state.Fee, state.ProtocolFee, zeroForOne)
	if err != nil {
		return nil, fmt.Errorf("uniswapv4: quote: %w", err)
	}
	amountOut, err := math.QuoteV3ExactIn(math.V3State{
		SqrtPriceX96: state.SqrtPriceX96,
		Tick:         state.Tick,
		Liquidity:    state.Liquidity,
		FeePips:      feePips,
		TickSpacing:  state.TickSpacing,
		Bitmap:       deep.Bitmap,
		Ticks:        deep.Ticks,
		WindowMin:    deep.WindowMin,
		WindowMax:    deep.WindowMax,
	}, zeroForOne, amountIn)
	if err != nil {
		return nil, fmt.Errorf("uniswapv4: quote: %w", err)
	}
	return amountOut, nil
}

// ExactInputMonotone certifies the fixed-fee curve used by this adapter.
func (a *Adapter) ExactInputMonotone(state pool.State) bool {
	return state.Key.Hooks == (domain.Address{})
}
