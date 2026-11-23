package math

import (
	"fmt"
	"math/big"
)

// NextSqrtPriceFromAmount0In returns the sqrt price after adding amountIn of
// token0, rounded up so the result never passes the target. amountIn is the
// amount after fees. Inputs are not mutated.
func NextSqrtPriceFromAmount0In(sqrtPriceX96, liquidity, amountIn *big.Int) (*big.Int, error) {
	if err := checkPositive("sqrt price", sqrtPriceX96); err != nil {
		return nil, err
	}
	if err := checkPositive("liquidity", liquidity); err != nil {
		return nil, err
	}
	if err := checkNonNegative("amount in", amountIn); err != nil {
		return nil, err
	}
	if amountIn.Sign() == 0 {
		return new(big.Int).Set(sqrtPriceX96), nil
	}

	numerator1 := new(big.Int).Lsh(liquidity, Q96Bits)
	product := new(big.Int).Mul(amountIn, sqrtPriceX96)
	if product.Cmp(uint256Max) <= 0 {
		denominator := new(big.Int).Add(numerator1, product)
		if denominator.Cmp(uint256Max) <= 0 {
			return mulDivRoundingUp(numerator1, sqrtPriceX96, denominator)
		}
	}

	// Fall back to the algebraically equivalent form that cannot overflow the
	// intermediate product.
	fallbackDenominator := new(big.Int).Add(new(big.Int).Div(numerator1, sqrtPriceX96), amountIn)
	return divRoundingUp(numerator1, fallbackDenominator)
}

// NextSqrtPriceFromAmount1In returns the sqrt price after adding amountIn of
// token1, rounded down so the result never passes the target. amountIn is the
// amount after fees. Inputs are not mutated.
func NextSqrtPriceFromAmount1In(sqrtPriceX96, liquidity, amountIn *big.Int) (*big.Int, error) {
	if err := checkPositive("sqrt price", sqrtPriceX96); err != nil {
		return nil, err
	}
	if err := checkPositive("liquidity", liquidity); err != nil {
		return nil, err
	}
	if err := checkNonNegative("amount in", amountIn); err != nil {
		return nil, err
	}
	if amountIn.Sign() == 0 {
		return new(big.Int).Set(sqrtPriceX96), nil
	}

	var quotient *big.Int
	if amountIn.Cmp(uint160Max) <= 0 {
		quotient = new(big.Int).Div(new(big.Int).Lsh(amountIn, Q96Bits), liquidity)
	} else {
		var err error
		quotient, err = MulDiv(amountIn, Q96, liquidity)
		if err != nil {
			return nil, fmt.Errorf("next sqrt price from amount1: %w", err)
		}
	}

	next := new(big.Int).Add(sqrtPriceX96, quotient)
	if next.Cmp(uint160Max) > 0 {
		return nil, fmt.Errorf("%w: sqrt price overflows uint160", ErrSqrtPriceOutOfRange)
	}
	return next, nil
}

// NextSqrtPriceFromAmount0Out returns the sqrt price after removing amountOut
// of token0, rounded up so the output never exceeds amountOut. Inputs are not
// mutated.
func NextSqrtPriceFromAmount0Out(sqrtPriceX96, liquidity, amountOut *big.Int) (*big.Int, error) {
	if err := checkPositive("sqrt price", sqrtPriceX96); err != nil {
		return nil, err
	}
	if err := checkPositive("liquidity", liquidity); err != nil {
		return nil, err
	}
	if err := checkPositive("amount out", amountOut); err != nil {
		return nil, err
	}

	numerator1 := new(big.Int).Lsh(liquidity, Q96Bits)
	product := new(big.Int).Mul(amountOut, sqrtPriceX96)
	denominator := new(big.Int).Sub(numerator1, product)
	if denominator.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount out %s exhausts token0 reserves", ErrSqrtPriceOutOfRange, amountOut)
	}
	return mulDivRoundingUp(numerator1, sqrtPriceX96, denominator)
}

// NextSqrtPriceFromAmount1Out returns the sqrt price after removing amountOut
// of token1, rounded down (the quotient is rounded up) so the output never
// exceeds amountOut. Inputs are not mutated.
func NextSqrtPriceFromAmount1Out(sqrtPriceX96, liquidity, amountOut *big.Int) (*big.Int, error) {
	if err := checkPositive("sqrt price", sqrtPriceX96); err != nil {
		return nil, err
	}
	if err := checkPositive("liquidity", liquidity); err != nil {
		return nil, err
	}
	if err := checkPositive("amount out", amountOut); err != nil {
		return nil, err
	}

	quotient, err := divRoundingUp(new(big.Int).Lsh(amountOut, Q96Bits), liquidity)
	if err != nil {
		return nil, fmt.Errorf("next sqrt price from amount1 out: %w", err)
	}
	next := new(big.Int).Sub(sqrtPriceX96, quotient)
	if next.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount out %s exhausts token1 reserves", ErrSqrtPriceOutOfRange, amountOut)
	}
	return next, nil
}

// amount0Delta returns the token0 amount between two sqrt prices. roundUp
// selects the direction used by exact-input amounts and exact-output outputs.
func amount0Delta(sqrtA, sqrtB, liquidity *big.Int, roundUp bool) (*big.Int, error) {
	if err := checkPositive("sqrt price a", sqrtA); err != nil {
		return nil, err
	}
	if err := checkPositive("sqrt price b", sqrtB); err != nil {
		return nil, err
	}
	if err := checkNonNegative("liquidity", liquidity); err != nil {
		return nil, err
	}

	a, b := sqrtA, sqrtB
	if a.Cmp(b) > 0 {
		a, b = b, a
	}

	numerator1 := new(big.Int).Lsh(liquidity, Q96Bits)
	numerator2 := new(big.Int).Sub(b, a)
	if roundUp {
		inner, err := mulDivRoundingUp(numerator1, numerator2, b)
		if err != nil {
			return nil, fmt.Errorf("amount0 delta: %w", err)
		}
		return divRoundingUp(inner, a)
	}
	inner, err := MulDiv(numerator1, numerator2, b)
	if err != nil {
		return nil, fmt.Errorf("amount0 delta: %w", err)
	}
	return new(big.Int).Div(inner, a), nil
}

// amount1Delta returns the token1 amount between two sqrt prices.
func amount1Delta(sqrtA, sqrtB, liquidity *big.Int, roundUp bool) (*big.Int, error) {
	if err := checkPositive("sqrt price a", sqrtA); err != nil {
		return nil, err
	}
	if err := checkPositive("sqrt price b", sqrtB); err != nil {
		return nil, err
	}
	if err := checkNonNegative("liquidity", liquidity); err != nil {
		return nil, err
	}

	a, b := sqrtA, sqrtB
	if a.Cmp(b) > 0 {
		a, b = b, a
	}
	delta := new(big.Int).Sub(b, a)
	if roundUp {
		return mulDivRoundingUp(liquidity, delta, Q96)
	}
	return MulDiv(liquidity, delta, Q96)
}
