package math

import (
	"fmt"
	"math/big"

	"github.com/holiman/uint256"
)

// swapV3ExactIn256 is SwapV3ExactInWithLimit in fixed-width arithmetic with
// the protocol's integer widths: amounts below 2^256 and liquidity below
// 2^128, which covers every state a deployed pool can hold. Every operation
// reproduces the arbitrary-precision path exactly; where an argument, the
// state or an intermediate does not fit those widths, or the state or limit is
// invalid, it reports handled=false and the caller reruns the swap in
// arbitrary precision, which returns the canonical result or error.
func swapV3ExactIn256(state V3State, zeroForOne bool, amountIn, sqrtPriceLimitX96 *big.Int) (SwapResult, bool, error) {
	var price, liquidity, remaining, limit uint256.Int
	if !tickMathTablesOK || amountIn == nil || amountIn.Sign() <= 0 || !setU256FromBig(&remaining, amountIn, 256) {
		return SwapResult{}, false, nil
	}
	if !loadV3State256(state, &price, &liquidity) {
		return SwapResult{}, false, nil
	}
	if !resolveSqrtPriceLimit256(&limit, &price, zeroForOne, sqrtPriceLimitX96) {
		return SwapResult{}, false, nil
	}

	var amountOut, amountInUsed, start, nextTickPrice, target, consumed uint256.Int
	var step swapStep256
	tick := state.Tick
	crossed := make([]int32, 0)

	for !remaining.IsZero() && !price.Eq(&limit) {
		start = price
		tickStart := tick

		tickNext, initialized := TickBitmapNextInitialized(state.Bitmap, tick, state.TickSpacing, zeroForOne)
		tickNext = max(MinTick, min(MaxTick, tickNext))
		cachedSqrtRatioAtTick(&nextTickPrice, tickNext)

		target = nextTickPrice
		if zeroForOne {
			if target.Lt(&limit) {
				target = limit
			}
		} else if target.Gt(&limit) {
			target = limit
		}

		if !step.compute(&price, &target, &liquidity, &remaining, state.FeePips) {
			return SwapResult{}, false, nil
		}

		if _, overflow := consumed.AddOverflow(&step.amountIn, &step.fee); overflow {
			return SwapResult{}, false, nil
		}
		if remaining.Lt(&consumed) {
			return SwapResult{}, true, fmt.Errorf("%w: swap step consumed more input than remaining", ErrInvalidState)
		}
		remaining.Sub(&remaining, &consumed)
		if _, overflow := amountInUsed.AddOverflow(&amountInUsed, &consumed); overflow {
			return SwapResult{}, false, nil
		}
		if _, overflow := amountOut.AddOverflow(&amountOut, &step.amountOut); overflow {
			return SwapResult{}, false, nil
		}
		price = step.next

		if price.Eq(&nextTickPrice) {
			if initialized {
				info, ok := state.Ticks[tickNext]
				if !ok || info.LiquidityNet == nil {
					return SwapResult{}, true, fmt.Errorf("%w: tick %d", ErrMissingTick, tickNext)
				}
				handled, negative := crossTick256(&liquidity, info.LiquidityNet, zeroForOne)
				if !handled {
					return SwapResult{}, false, nil
				}
				if negative {
					return SwapResult{}, true, fmt.Errorf("%w: negative liquidity after crossing tick %d", ErrInvalidState, tickNext)
				}
				crossed = append(crossed, tickNext)
			}
			if zeroForOne {
				tick = tickNext - 1
			} else {
				tick = tickNext
			}
		} else if !price.Eq(&start) {
			if price.Lt(&minSqrtRatio256) || !price.Lt(&maxSqrtRatio256) {
				return SwapResult{}, false, nil
			}
			tick = tickAtSqrtRatio(&price)
		}

		if price.Eq(&start) && tick == tickStart && consumed.IsZero() {
			return SwapResult{}, true, fmt.Errorf("%w: swap loop made no progress", ErrInvalidState)
		}
	}

	if err := checkWindow(state, zeroForOne, tick); err != nil {
		return SwapResult{}, true, err
	}

	results := new(bigResults)
	return SwapResult{
		AmountOut:    results.set(0, &amountOut),
		AmountInUsed: results.set(1, &amountInUsed),
		SqrtPriceX96: results.set(2, &price),
		Liquidity:    results.set(3, &liquidity),
		Tick:         tick,
		Crossed:      crossed,
		HitLimit:     !remaining.IsZero() && price.Eq(&limit),
	}, true, nil
}

// loadV3State256 converts the price and liquidity of a state that passes
// validateV3State and whose liquidity fits 128 bits. It reports false for any
// other state.
func loadV3State256(state V3State, price, liquidity *uint256.Int) bool {
	if state.SqrtPriceX96 == nil || state.Liquidity == nil || state.Liquidity.Sign() <= 0 {
		return false
	}
	if !setU256FromBig(price, state.SqrtPriceX96, 160) || !setU256FromBig(liquidity, state.Liquidity, 128) {
		return false
	}
	if state.Tick < MinTick || state.Tick > MaxTick || state.TickSpacing <= 0 || state.FeePips >= feePipsDenominator {
		return false
	}
	if price.Lt(&minSqrtRatio256) || !price.Lt(&maxSqrtRatio256) {
		return false
	}
	if (state.WindowMin != 0 || state.WindowMax != 0) && state.WindowMax < state.WindowMin {
		return false
	}
	var bound uint256.Int
	cachedSqrtRatioAtTick(&bound, state.Tick)
	if price.Lt(&bound) {
		return false
	}
	if state.Tick < MaxTick {
		cachedSqrtRatioAtTick(&bound, state.Tick+1)
		if !price.Lt(&bound) {
			return false
		}
	}
	return true
}

// resolveSqrtPriceLimit256 is resolveSqrtPriceLimit for a validated price. It
// reports false for a limit that resolveSqrtPriceLimit rejects.
func resolveSqrtPriceLimit256(limit, price *uint256.Int, zeroForOne bool, requested *big.Int) bool {
	if requested == nil || requested.Sign() == 0 {
		if zeroForOne {
			limit.AddUint64(&minSqrtRatio256, 1)
		} else {
			limit.SubUint64(&maxSqrtRatio256, 1)
		}
		return true
	}
	if !setU256FromBig(limit, requested, 160) {
		return false
	}
	if zeroForOne {
		return limit.Gt(&minSqrtRatio256) && limit.Lt(price)
	}
	return limit.Lt(&maxSqrtRatio256) && limit.Gt(price)
}

// crossTick256 applies a tick's liquidityNet in the swap direction. handled is
// false when the result could leave 128 bits; negative reports a crossing that
// would drive liquidity below zero, leaving liquidity unchanged.
func crossTick256(liquidity *uint256.Int, liquidityNet *big.Int, zeroForOne bool) (handled, negative bool) {
	if liquidityNet.BitLen() > 128 {
		return false, false
	}
	// SetFromBig stores a negative value in two's complement.
	var magnitude uint256.Int
	magnitude.SetFromBig(liquidityNet)
	netNegative := liquidityNet.Sign() < 0
	if netNegative {
		magnitude.Neg(&magnitude)
	}
	// Moving down subtracts liquidityNet; moving up adds it.
	subtract := zeroForOne != netNegative
	if subtract {
		if liquidity.Lt(&magnitude) {
			return true, true
		}
		liquidity.Sub(liquidity, &magnitude)
		return true, false
	}
	liquidity.Add(liquidity, &magnitude)
	return liquidity.BitLen() <= 128, false
}

// swapStep256 is one exact-input SwapMath.computeSwapStep. amountIn excludes
// the fee.
type swapStep256 struct {
	next, amountIn, amountOut, fee uint256.Int
}

// compute mirrors swapWorkspace.computeStep. It requires liquidity below
// 2^128 and prices in the protocol range, and reports false when an
// intermediate leaves 256 bits or the next price leaves 160 bits, both cases
// the arbitrary-precision path reports or computes itself.
func (s *swapStep256) compute(current, target, liquidity, remaining *uint256.Int, feePips uint32) bool {
	feeComplement := uint256.Int{uint64(feePipsDenominator - feePips)}
	var remainingLessFee, toTarget uint256.Int
	if !mulDiv256(&remainingLessFee, remaining, &feeComplement, u256FeeDivisor, false) {
		return false
	}
	zeroForOne := !current.Lt(target)
	if !amountDelta256(&toTarget, current, target, liquidity, zeroForOne, true) {
		return false
	}
	reached := !remainingLessFee.Lt(&toTarget)
	if reached {
		s.next = *target
	} else {
		if !nextSqrtPriceFromInput256(&s.next, current, liquidity, &remainingLessFee, zeroForOne) {
			return false
		}
		reached = s.next.Eq(target)
	}
	if reached {
		s.amountIn = toTarget
	} else if !amountDelta256(&s.amountIn, current, &s.next, liquidity, zeroForOne, true) {
		return false
	}
	if !amountDelta256(&s.amountOut, current, &s.next, liquidity, !zeroForOne, false) {
		return false
	}
	if reached {
		feeAmount := uint256.Int{uint64(feePips)}
		return mulDiv256(&s.fee, &s.amountIn, &feeAmount, &feeComplement, true)
	}
	_, underflow := s.fee.SubOverflow(remaining, &s.amountIn)
	return !underflow
}

// amountDelta256 is SqrtPriceMath.getAmount0Delta (token0) or getAmount1Delta
// between two positive prices for liquidity below 2^128. The token0 amount
// keeps the protocol's two rounding stages.
func amountDelta256(z, a, b, liquidity *uint256.Int, token0, roundUp bool) bool {
	if a.Gt(b) {
		a, b = b, a
	}
	if a.IsZero() {
		return false
	}
	var delta uint256.Int
	delta.Sub(b, a)
	if !token0 {
		// Dividing by Q96 is a shift whenever the product fits 256 bits.
		var product uint256.Int
		if _, overflow := product.MulOverflow(liquidity, &delta); overflow {
			return mulDiv256(z, liquidity, &delta, u256Q96, roundUp)
		}
		inexact := product[0] != 0 || uint32(product[1]) != 0
		z.Rsh(&product, Q96Bits)
		if roundUp && inexact {
			z.AddUint64(z, 1)
		}
		return true
	}
	var numerator, inner uint256.Int
	numerator.Lsh(liquidity, Q96Bits)
	if !mulDiv256(&inner, &numerator, &delta, b, roundUp) {
		return false
	}
	div256(z, &inner, a, roundUp)
	return true
}

// nextSqrtPriceFromInput256 is SqrtPriceMath.getNextSqrtPriceFromInput for a
// non-zero liquidity below 2^128, including the token0 overflow fallback. It
// reports false when the token1 result leaves 160 bits.
func nextSqrtPriceFromInput256(z, current, liquidity, amount *uint256.Int, zeroForOne bool) bool {
	if amount.IsZero() {
		z.Set(current)
		return true
	}
	if !zeroForOne {
		var quotient uint256.Int
		if !mulDiv256(&quotient, amount, u256Q96, liquidity, false) {
			return false
		}
		if _, overflow := z.AddOverflow(&quotient, current); overflow {
			return false
		}
		return !z.Gt(u256Uint160Max)
	}
	var numerator, product, denominator uint256.Int
	numerator.Lsh(liquidity, Q96Bits)
	if _, overflow := product.MulOverflow(amount, current); !overflow {
		if _, overflow := denominator.AddOverflow(&numerator, &product); !overflow {
			return mulDiv256(z, &numerator, current, &denominator, true)
		}
	}
	// The protocol's fallback when amount*price or the denominator overflows.
	denominator.Div(&numerator, current)
	if _, overflow := denominator.AddOverflow(&denominator, amount); overflow {
		return false
	}
	div256(z, &numerator, &denominator, true)
	return true
}
