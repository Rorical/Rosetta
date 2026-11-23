package math

import (
	"fmt"
	"math/big"
)

const (
	// MinTick and MaxTick are the Uniswap V3 tick bounds.
	MinTick int32 = -887272
	MaxTick int32 = 887272

	// Q96Bits is the number of fractional bits in Q96.96 prices.
	Q96Bits = 96

	// feePipsDenominator is 100% expressed in hundredths of a basis point.
	feePipsDenominator = 1_000_000
)

// Q96 is 2^96, the fixed-point scale for V3 sqrt prices and amounts.
//
// Q96 is shared and must never be mutated. Functions in this package only use
// it as an operand and return freshly allocated values.
var Q96 = new(big.Int).Lsh(big.NewInt(1), Q96Bits)

var (
	one                   = big.NewInt(1)
	uint160Max            = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), one)
	uint256Max            = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), one)
	feePipsDenominatorBig = big.NewInt(feePipsDenominator)
)

// TickInfo is the liquidity stored at an initialized tick. LiquidityNet is
// added when price moves up across the tick and subtracted when it moves down.
type TickInfo struct {
	LiquidityGross *big.Int
	LiquidityNet   *big.Int
}

// V3State is the pre-swap state needed to replay one exact-input swap. Bitmap
// maps a bitmap word position to its 256-bit word; a missing word is zero.
// Ticks maps initialized tick indices to their liquidity. WindowMin and
// WindowMax are the inclusive tick bounds the bitmap and ticks cover; when
// either is non-zero, a swap whose final tick leaves the window is rejected
// because liquidity outside it is unknown. Zero/zero means the window is
// unknown and no check is made.
type V3State struct {
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
	FeePips      uint32
	TickSpacing  int32
	Bitmap       map[int16]*big.Int
	Ticks        map[int32]TickInfo
	WindowMin    int32
	WindowMax    int32
}

// SwapResult is the outcome of an exact-input or exact-output swap.
// AmountInUsed is the gross input including fee. For exact input, AmountOut is
// the delivered output; for exact output, AmountOut is the requested output and
// AmountInUsed is the computed gross input. Crossed lists initialized ticks
// crossed in order. HitLimit reports that input (exact input) or output (exact
// output) remained when the swap reached the price limit, either the caller's
// or the full-range bound. Liquidity is the active liquidity after the swap and
// never aliases the pre-state.
type SwapResult struct {
	AmountOut    *big.Int
	AmountInUsed *big.Int
	SqrtPriceX96 *big.Int
	Liquidity    *big.Int
	Tick         int32
	Crossed      []int32
	HitLimit     bool
}

// ClosedFormLeg describes one pool of a two-leg cycle by the reserves of the
// input and output token at the current price.
type ClosedFormLeg struct {
	InReserve  *big.Int
	OutReserve *big.Int
	FeePips    uint32
}

func checkPositive(name string, v *big.Int) error {
	if v == nil {
		return fmt.Errorf("%w: %s is nil", ErrInvalidArgument, name)
	}
	if v.Sign() <= 0 {
		return fmt.Errorf("%w: %s must be positive", ErrInvalidArgument, name)
	}
	return nil
}

func checkNonNegative(name string, v *big.Int) error {
	if v == nil {
		return fmt.Errorf("%w: %s is nil", ErrInvalidArgument, name)
	}
	if v.Sign() < 0 {
		return fmt.Errorf("%w: %s must be non-negative", ErrInvalidArgument, name)
	}
	return nil
}
