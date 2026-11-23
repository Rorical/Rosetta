package math

import (
	"fmt"
	"math/big"
)

// SwapV3ExactIn replays one Uniswap V3 exact-input swap over the full price
// range against a fixed pre-state. It is SwapV3ExactInWithLimit with a nil
// limit.
func SwapV3ExactIn(state V3State, zeroForOne bool, amountIn *big.Int) (SwapResult, error) {
	return SwapV3ExactInWithLimit(state, zeroForOne, amountIn, nil)
}

// SwapV3ExactInWithLimit replays one exact-input swap that stops at
// sqrtPriceLimitX96. A nil or zero limit means the full price range. A non-zero
// limit must sit strictly on the swap's side of the current price and inside
// the representable range: below the current price and above the minimum sqrt
// price for zeroForOne, above the current price and below the maximum
// otherwise. Any other limit returns ErrInvalidArgument. When the limit stops
// the swap with input remaining, HitLimit is set and SqrtPriceX96 is exactly
// the limit.
//
// When state.WindowMin/WindowMax are known, a quote that ends outside the
// loaded window returns ErrOutOfWindow, or ErrCapacity once the exit side
// covered CoverageTicks, instead of a price built from absent
// liquidity; an unknown zero window is not enforced. Missing liquidity for an
// initialized tick is an error, never zero. State and amountIn are not mutated.
func SwapV3ExactInWithLimit(state V3State, zeroForOne bool, amountIn, sqrtPriceLimitX96 *big.Int) (SwapResult, error) {
	if result, handled, err := swapV3ExactIn256(state, zeroForOne, amountIn, sqrtPriceLimitX96); handled {
		return result, err
	}
	return swapV3ExactInBig(state, zeroForOne, amountIn, sqrtPriceLimitX96)
}

// swapV3ExactInBig is SwapV3ExactInWithLimit in arbitrary precision. It
// covers the inputs the fixed-width path declines: invalid arguments and
// states, and values wider than a deployed pool can hold.
func swapV3ExactInBig(state V3State, zeroForOne bool, amountIn, sqrtPriceLimitX96 *big.Int) (SwapResult, error) {
	if err := checkPositive("amount in", amountIn); err != nil {
		return SwapResult{}, err
	}
	if err := validateV3State(state); err != nil {
		return SwapResult{}, err
	}
	limit, err := resolveSqrtPriceLimit(state, zeroForOne, sqrtPriceLimitX96)
	if err != nil {
		return SwapResult{}, err
	}

	sqrtPrice := new(big.Int).Set(state.SqrtPriceX96)
	liquidity := new(big.Int).Set(state.Liquidity)
	tick := state.Tick
	remaining := new(big.Int).Set(amountIn)
	amountOut := new(big.Int)
	amountInUsed := new(big.Int)
	crossed := make([]int32, 0)
	work := swapWorkspaces.Get().(*swapWorkspace)
	defer swapWorkspaces.Put(work)

	for remaining.Sign() != 0 && sqrtPrice.Cmp(limit) != 0 {
		sqrtStart := &work.start
		sqrtStart.Set(sqrtPrice)
		tickStart := tick

		tickNext, initialized := TickBitmapNextInitialized(state.Bitmap, tick, state.TickSpacing, zeroForOne)
		if tickNext < MinTick {
			tickNext = MinTick
		}
		if tickNext > MaxTick {
			tickNext = MaxTick
		}
		sqrtNextTick, err := tickPrice(tickNext)
		if err != nil {
			return SwapResult{}, fmt.Errorf("swap v3: next tick: %w", err)
		}

		target := &work.target
		target.Set(sqrtNextTick)
		if zeroForOne {
			if target.Cmp(limit) < 0 {
				target.Set(limit)
			}
		} else if target.Cmp(limit) > 0 {
			target.Set(limit)
		}

		step, err := work.computeStep(sqrtPrice, target, liquidity, remaining, state.FeePips)
		if err != nil {
			return SwapResult{}, fmt.Errorf("swap v3: %w", err)
		}

		consumed := &work.consumed
		consumed.Add(step.amountIn, step.fee)
		remaining.Sub(remaining, consumed)
		if remaining.Sign() < 0 {
			return SwapResult{}, fmt.Errorf("%w: swap step consumed more input than remaining", ErrInvalidState)
		}
		amountInUsed.Add(amountInUsed, consumed)
		amountOut.Add(amountOut, step.amountOut)
		sqrtPrice.Set(step.sqrtPriceNext)

		if sqrtPrice.Cmp(sqrtNextTick) == 0 {
			if initialized {
				info, ok := state.Ticks[tickNext]
				if !ok || info.LiquidityNet == nil {
					return SwapResult{}, fmt.Errorf("%w: tick %d", ErrMissingTick, tickNext)
				}
				if zeroForOne {
					liquidity.Sub(liquidity, info.LiquidityNet)
				} else {
					liquidity.Add(liquidity, info.LiquidityNet)
				}
				if liquidity.Sign() < 0 {
					return SwapResult{}, fmt.Errorf("%w: negative liquidity after crossing tick %d", ErrInvalidState, tickNext)
				}
				crossed = append(crossed, tickNext)
			}
			if zeroForOne {
				tick = tickNext - 1
			} else {
				tick = tickNext
			}
		} else if sqrtPrice.Cmp(sqrtStart) != 0 {
			tick, err = SqrtPriceX96ToTick(sqrtPrice)
			if err != nil {
				return SwapResult{}, fmt.Errorf("swap v3: final price: %w", err)
			}
		}

		if sqrtPrice.Cmp(sqrtStart) == 0 && tick == tickStart && consumed.Sign() == 0 {
			return SwapResult{}, fmt.Errorf("%w: swap loop made no progress", ErrInvalidState)
		}
	}

	if err := checkWindow(state, zeroForOne, tick); err != nil {
		return SwapResult{}, err
	}

	return SwapResult{
		AmountOut:    amountOut,
		AmountInUsed: amountInUsed,
		SqrtPriceX96: sqrtPrice,
		Liquidity:    liquidity,
		Tick:         tick,
		Crossed:      crossed,
		HitLimit:     remaining.Sign() != 0 && sqrtPrice.Cmp(limit) == 0,
	}, nil
}

// SwapV3ExactOut replays one Uniswap V3 exact-output swap over the full price
// range against a fixed pre-state. zeroForOne means token0 in and token1 out;
// otherwise token1 in and token0 out. AmountOut is the requested output and
// AmountInUsed is the gross input including fee. HitLimit reports that output
// remained when the full-range price limit was reached; AmountOut still reports
// the request. It is SwapV3ExactOutWithLimit with a nil limit.
//
// Window handling matches SwapV3ExactIn: a known window that the final tick
// leaves returns ErrOutOfWindow or ErrCapacity.
func SwapV3ExactOut(state V3State, zeroForOne bool, amountOut *big.Int) (SwapResult, error) {
	return SwapV3ExactOutWithLimit(state, zeroForOne, amountOut, nil)
}

// SwapV3ExactOutWithLimit replays one exact-output swap that stops at
// sqrtPriceLimitX96. A nil or zero limit means the full price range; any other
// limit must sit on the swap's side of the current price and inside the
// representable range, matching SwapV3ExactInWithLimit. When the limit stops
// the swap with output remaining, HitLimit is set, SqrtPriceX96 is exactly the
// limit, and AmountOut still reports the request.
func SwapV3ExactOutWithLimit(state V3State, zeroForOne bool, amountOut, sqrtPriceLimitX96 *big.Int) (SwapResult, error) {
	if err := checkPositive("amount out", amountOut); err != nil {
		return SwapResult{}, err
	}
	if err := validateV3State(state); err != nil {
		return SwapResult{}, err
	}
	limit, err := resolveSqrtPriceLimit(state, zeroForOne, sqrtPriceLimitX96)
	if err != nil {
		return SwapResult{}, err
	}

	sqrtPrice := new(big.Int).Set(state.SqrtPriceX96)
	liquidity := new(big.Int).Set(state.Liquidity)
	tick := state.Tick
	remaining := new(big.Int).Set(amountOut)
	amountInUsed := new(big.Int)
	crossed := make([]int32, 0)

	for remaining.Sign() != 0 && sqrtPrice.Cmp(limit) != 0 {
		sqrtStart := new(big.Int).Set(sqrtPrice)
		tickStart := tick

		tickNext, initialized := TickBitmapNextInitialized(state.Bitmap, tick, state.TickSpacing, zeroForOne)
		if tickNext < MinTick {
			tickNext = MinTick
		}
		if tickNext > MaxTick {
			tickNext = MaxTick
		}
		sqrtNextTick, err := tickPrice(tickNext)
		if err != nil {
			return SwapResult{}, fmt.Errorf("swap v3 exact out: next tick: %w", err)
		}

		target := new(big.Int).Set(sqrtNextTick)
		if zeroForOne {
			if target.Cmp(limit) < 0 {
				target.Set(limit)
			}
		} else if target.Cmp(limit) > 0 {
			target.Set(limit)
		}

		step, err := computeSwapStepExactOut(zeroForOne, sqrtPrice, target, liquidity, remaining, state.FeePips)
		if err != nil {
			return SwapResult{}, fmt.Errorf("swap v3 exact out: %w", err)
		}

		remaining.Sub(remaining, step.amountOut)
		if remaining.Sign() < 0 {
			return SwapResult{}, fmt.Errorf("%w: swap step consumed more output than remaining", ErrInvalidState)
		}
		consumed := new(big.Int).Add(step.amountIn, step.fee)
		amountInUsed.Add(amountInUsed, consumed)
		sqrtPrice = step.sqrtPriceNext

		if sqrtPrice.Cmp(sqrtNextTick) == 0 {
			if initialized {
				info, ok := state.Ticks[tickNext]
				if !ok || info.LiquidityNet == nil {
					return SwapResult{}, fmt.Errorf("%w: tick %d", ErrMissingTick, tickNext)
				}
				net := new(big.Int).Set(info.LiquidityNet)
				if zeroForOne {
					net.Neg(net)
				}
				liquidity.Add(liquidity, net)
				if liquidity.Sign() < 0 {
					return SwapResult{}, fmt.Errorf("%w: negative liquidity after crossing tick %d", ErrInvalidState, tickNext)
				}
				crossed = append(crossed, tickNext)
			}
			if zeroForOne {
				tick = tickNext - 1
			} else {
				tick = tickNext
			}
		} else if sqrtPrice.Cmp(sqrtStart) != 0 {
			tick, err = SqrtPriceX96ToTick(sqrtPrice)
			if err != nil {
				return SwapResult{}, fmt.Errorf("swap v3 exact out: final price: %w", err)
			}
		}

		if sqrtPrice.Cmp(sqrtStart) == 0 && tick == tickStart && step.amountOut.Sign() == 0 {
			return SwapResult{}, fmt.Errorf("%w: swap loop made no progress", ErrInvalidState)
		}
	}

	if err := checkWindow(state, zeroForOne, tick); err != nil {
		return SwapResult{}, err
	}

	return SwapResult{
		AmountOut:    new(big.Int).Set(amountOut),
		AmountInUsed: amountInUsed,
		SqrtPriceX96: sqrtPrice,
		Liquidity:    liquidity,
		Tick:         tick,
		Crossed:      crossed,
		HitLimit:     remaining.Sign() != 0 && sqrtPrice.Cmp(limit) == 0,
	}, nil
}

type swapStep struct {
	sqrtPriceNext *big.Int
	amountIn      *big.Int
	amountOut     *big.Int
	fee           *big.Int
}

// resolveSqrtPriceLimit returns the price the swap may not cross. A nil or zero
// request means the full-range bound on the swap's side; anything else must
// leave room to move: below the current price and above the minimum sqrt price
// for zeroForOne, above the current price and below the maximum otherwise.
func resolveSqrtPriceLimit(state V3State, zeroForOne bool, requested *big.Int) (*big.Int, error) {
	if requested == nil || requested.Sign() == 0 {
		if zeroForOne {
			return new(big.Int).Add(minSqrtRatioX96, one), nil
		}
		return new(big.Int).Sub(maxSqrtRatioX96, one), nil
	}

	if zeroForOne {
		// The price may fall but must stay above the minimum the tick math
		// can represent.
		if requested.Cmp(minSqrtRatioX96) <= 0 || requested.Cmp(state.SqrtPriceX96) >= 0 {
			return nil, fmt.Errorf("%w: zero-for-one sqrt price limit %s must be above %s and below the current price %s",
				ErrInvalidArgument, requested, minSqrtRatioX96, state.SqrtPriceX96)
		}
		return new(big.Int).Set(requested), nil
	}
	if requested.Cmp(maxSqrtRatioX96) >= 0 || requested.Cmp(state.SqrtPriceX96) <= 0 {
		return nil, fmt.Errorf("%w: one-for-zero sqrt price limit %s must be below %s and above the current price %s",
			ErrInvalidArgument, requested, maxSqrtRatioX96, state.SqrtPriceX96)
	}
	return new(big.Int).Set(requested), nil
}

// computeSwapStepExactOut is one exact-output step toward target. A partial
// step consumes exactly amountOutRemaining; amountIn excludes the fee.
func computeSwapStepExactOut(zeroForOne bool, sqrtCurrent, sqrtTarget, liquidity, amountOutRemaining *big.Int, feePips uint32) (swapStep, error) {
	var zero swapStep

	var amountOutAtTarget *big.Int
	var err error
	if zeroForOne {
		amountOutAtTarget, err = amount1Delta(sqrtTarget, sqrtCurrent, liquidity, false)
	} else {
		amountOutAtTarget, err = amount0Delta(sqrtCurrent, sqrtTarget, liquidity, false)
	}
	if err != nil {
		return zero, fmt.Errorf("compute swap step exact out: output to target: %w", err)
	}

	reachedTarget := amountOutRemaining.Cmp(amountOutAtTarget) >= 0
	next := new(big.Int).Set(sqrtTarget)
	if !reachedTarget {
		if zeroForOne {
			next, err = NextSqrtPriceFromAmount1Out(sqrtCurrent, liquidity, amountOutRemaining)
		} else {
			next, err = NextSqrtPriceFromAmount0Out(sqrtCurrent, liquidity, amountOutRemaining)
		}
		if err != nil {
			return zero, fmt.Errorf("compute swap step exact out: next price: %w", err)
		}
	}

	var amountIn *big.Int
	if zeroForOne {
		amountIn, err = amount0Delta(next, sqrtCurrent, liquidity, true)
	} else {
		amountIn, err = amount1Delta(sqrtCurrent, next, liquidity, true)
	}
	if err != nil {
		return zero, fmt.Errorf("compute swap step exact out: input: %w", err)
	}

	var amountOut *big.Int
	if reachedTarget {
		amountOut = amountOutAtTarget
	} else {
		// The next price was computed from amountOutRemaining, so this step
		// consumes exactly that output. Recomputing the delta could floor one
		// wei lower and leave residual output for another loop iteration.
		amountOut = new(big.Int).Set(amountOutRemaining)
	}

	feeComplement := big.NewInt(int64(feePipsDenominator - feePips))
	fee, err := mulDivRoundingUp(amountIn, big.NewInt(int64(feePips)), feeComplement)
	if err != nil {
		return zero, fmt.Errorf("compute swap step exact out: fee: %w", err)
	}

	return swapStep{
		sqrtPriceNext: next,
		amountIn:      amountIn,
		amountOut:     amountOut,
		fee:           fee,
	}, nil
}

func validateV3State(state V3State) error {
	if err := checkPositive("sqrt price", state.SqrtPriceX96); err != nil {
		return err
	}
	if err := checkPositive("liquidity", state.Liquidity); err != nil {
		return err
	}
	if state.Tick < MinTick || state.Tick > MaxTick {
		return fmt.Errorf("%w: tick %d", ErrTickOutOfRange, state.Tick)
	}
	if state.TickSpacing <= 0 {
		return fmt.Errorf("%w: tick spacing %d", ErrInvalidState, state.TickSpacing)
	}
	if state.FeePips >= feePipsDenominator {
		return fmt.Errorf("%w: fee %d pips", ErrInvalidState, state.FeePips)
	}
	if state.SqrtPriceX96.Cmp(minSqrtRatioX96) < 0 || state.SqrtPriceX96.Cmp(maxSqrtRatioX96) >= 0 {
		return fmt.Errorf("%w: sqrt price %s", ErrSqrtPriceOutOfRange, state.SqrtPriceX96)
	}
	if (state.WindowMin != 0 || state.WindowMax != 0) && state.WindowMax < state.WindowMin {
		return fmt.Errorf("%w: window [%d, %d] is reversed", ErrInvalidState, state.WindowMin, state.WindowMax)
	}

	lower, err := tickPrice(state.Tick)
	if err != nil {
		return fmt.Errorf("validate v3 state: %w", err)
	}
	if state.SqrtPriceX96.Cmp(lower) < 0 {
		return fmt.Errorf("%w: sqrt price %s is below tick %d", ErrInvalidState, state.SqrtPriceX96, state.Tick)
	}
	if state.Tick < MaxTick {
		upper, err := tickPrice(state.Tick + 1)
		if err != nil {
			return fmt.Errorf("validate v3 state: %w", err)
		}
		// Equal is valid: a zeroForOne swap ending on a tick leaves the
		// price there with tick one below (UniswapV3Pool: tickNext − 1).
		if state.SqrtPriceX96.Cmp(upper) > 0 {
			return fmt.Errorf("%w: sqrt price %s is above tick %d", ErrInvalidState, state.SqrtPriceX96, state.Tick+1)
		}
	}
	return nil
}

func floorDiv(a, b int32) int32 {
	quotient := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		quotient--
	}
	return quotient
}
