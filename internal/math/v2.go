package math

import (
	"fmt"
	"math/big"
)

// QuoteV2ExactIn returns the token output of a constant-product pool for a
// gross amountIn, applying feePips to the input. Reserves must be positive and
// amountIn positive. The result is floored and never reaches reserveOut.
func QuoteV2ExactIn(reserveIn, reserveOut *big.Int, feePips uint32, amountIn *big.Int) (*big.Int, error) {
	if err := checkPositive("reserve in", reserveIn); err != nil {
		return nil, err
	}
	if err := checkPositive("reserve out", reserveOut); err != nil {
		return nil, err
	}
	if err := checkPositive("amount in", amountIn); err != nil {
		return nil, err
	}
	if feePips >= feePipsDenominator {
		return nil, fmt.Errorf("%w: fee %d pips", ErrInvalidArgument, feePips)
	}

	amountInWithFee := new(big.Int).Mul(amountIn, big.NewInt(int64(feePipsDenominator-feePips)))
	numerator := new(big.Int).Mul(amountInWithFee, reserveOut)
	denominator := new(big.Int).Add(new(big.Int).Mul(reserveIn, feePipsDenominatorBig), amountInWithFee)
	return new(big.Int).Div(numerator, denominator), nil
}

// QuoteV2ExactOut returns the gross input a constant-product pool needs to
// deliver amountOut, applying feePips to the input. It rounds up, so the
// result produces at least amountOut when passed back to QuoteV2ExactIn.
// Reserves and amountOut must be positive, amountOut must stay below
// reserveOut, and feePips must be below 100%.
func QuoteV2ExactOut(reserveIn, reserveOut *big.Int, feePips uint32, amountOut *big.Int) (*big.Int, error) {
	if err := checkPositive("reserve in", reserveIn); err != nil {
		return nil, err
	}
	if err := checkPositive("reserve out", reserveOut); err != nil {
		return nil, err
	}
	if err := checkPositive("amount out", amountOut); err != nil {
		return nil, err
	}
	if feePips >= feePipsDenominator {
		return nil, fmt.Errorf("%w: fee %d pips", ErrInvalidArgument, feePips)
	}
	if amountOut.Cmp(reserveOut) >= 0 {
		return nil, fmt.Errorf("%w: amount out %s must be below reserve out %s",
			ErrInvalidArgument, amountOut, reserveOut)
	}

	numerator := new(big.Int).Mul(reserveIn, amountOut)
	numerator.Mul(numerator, feePipsDenominatorBig)
	denominator := new(big.Int).Sub(reserveOut, amountOut)
	denominator.Mul(denominator, big.NewInt(int64(feePipsDenominator-feePips)))
	return divRoundingUp(numerator, denominator)
}
