package math

import (
	"fmt"
	"math/big"
	"sync"
)

// Each exact-input swap owns its scratch integers until it returns. Only the
// final SwapResult escapes; no price, pool state or caller integer is retained.
type swapWorkspace struct {
	start, target, consumed                  big.Int
	next, amountIn, amountOut, fee           big.Int
	feeComplement, feePips, remainingLessFee big.Int
	toTarget, numerator, delta, product      big.Int
	denominator, remainder                   big.Int
}

var swapWorkspaces = sync.Pool{New: func() any { return new(swapWorkspace) }}

func (w *swapWorkspace) divide(dst, numerator, denominator *big.Int, roundUp bool) {
	dst.QuoRem(numerator, denominator, &w.remainder)
	if roundUp && w.remainder.Sign() != 0 {
		dst.Add(dst, one)
	}
}

// deltaAmount preserves both rounding stages of amount0Delta. Combining them
// into one rounded division would change protocol integer results.
func (w *swapWorkspace) deltaAmount(dst, a, b, liquidity *big.Int, token0, roundUp bool) {
	if a.Cmp(b) > 0 {
		a, b = b, a
	}
	w.delta.Sub(b, a)
	if token0 {
		w.numerator.Lsh(liquidity, Q96Bits)
		w.product.Mul(&w.numerator, &w.delta)
		w.divide(&w.product, &w.product, b, roundUp)
		w.divide(dst, &w.product, a, roundUp)
		return
	}
	w.product.Mul(liquidity, &w.delta)
	w.divide(dst, &w.product, Q96, roundUp)
}

func (w *swapWorkspace) nextPrice(current, liquidity, amount *big.Int, zeroForOne bool) error {
	if amount.Sign() == 0 {
		w.next.Set(current)
		return nil
	}
	if !zeroForOne {
		w.product.Lsh(amount, Q96Bits)
		w.next.Quo(&w.product, liquidity)
		w.next.Add(&w.next, current)
		if w.next.Cmp(uint160Max) > 0 {
			return fmt.Errorf("%w: sqrt price overflows uint160", ErrSqrtPriceOutOfRange)
		}
		return nil
	}
	w.numerator.Lsh(liquidity, Q96Bits)
	w.product.Mul(amount, current)
	w.denominator.Add(&w.numerator, &w.product)
	if w.product.Cmp(uint256Max) <= 0 && w.denominator.Cmp(uint256Max) <= 0 {
		w.product.Mul(&w.numerator, current)
		w.divide(&w.next, &w.product, &w.denominator, true)
		return nil
	}
	// Match the protocol's uint256 overflow fallback, including its rounding.
	w.denominator.Quo(&w.numerator, current)
	w.denominator.Add(&w.denominator, amount)
	w.divide(&w.next, &w.numerator, &w.denominator, true)
	return nil
}

// computeStep requires validated prices, fees and nonnegative liquidity. Its
// result borrows scratch values until the next step; the swap copies next price
// before advancing, and accumulates amounts into independently owned integers.
func (w *swapWorkspace) computeStep(current, target, liquidity, remaining *big.Int, feePips uint32) (swapStep, error) {
	w.feeComplement.SetUint64(uint64(feePipsDenominator - feePips))
	w.product.Mul(remaining, &w.feeComplement)
	w.remainingLessFee.Quo(&w.product, feePipsDenominatorBig)
	zeroForOne := current.Cmp(target) >= 0
	w.deltaAmount(&w.toTarget, current, target, liquidity, zeroForOne, true)
	reached := w.remainingLessFee.Cmp(&w.toTarget) >= 0
	if reached {
		w.next.Set(target)
	} else {
		if err := w.nextPrice(current, liquidity, &w.remainingLessFee, zeroForOne); err != nil {
			return swapStep{}, err
		}
		reached = w.next.Cmp(target) == 0
	}
	if reached {
		w.amountIn.Set(&w.toTarget)
	} else {
		w.deltaAmount(&w.amountIn, current, &w.next, liquidity, zeroForOne, true)
	}
	w.deltaAmount(&w.amountOut, current, &w.next, liquidity, !zeroForOne, false)
	if reached {
		w.feePips.SetUint64(uint64(feePips))
		w.product.Mul(&w.amountIn, &w.feePips)
		w.divide(&w.fee, &w.product, &w.feeComplement, true)
	} else {
		w.fee.Sub(remaining, &w.amountIn)
	}
	return swapStep{sqrtPriceNext: &w.next, amountIn: &w.amountIn, amountOut: &w.amountOut, fee: &w.fee}, nil
}
