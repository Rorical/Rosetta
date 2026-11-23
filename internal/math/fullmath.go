package math

import (
	"fmt"
	"math/big"
)

// MulDiv returns floor(a*b/m). Inputs must be non-negative and m non-zero.
// Inputs are never mutated; the result is a new big.Int.
func MulDiv(a, b, m *big.Int) (*big.Int, error) {
	if err := checkMulDivOperands(a, b, m); err != nil {
		return nil, err
	}
	product := new(big.Int).Mul(a, b)
	return product.Quo(product, m), nil
}

// mulDivRoundingUp returns ceil(a*b/m). Inputs must be non-negative and m
// non-zero.
func mulDivRoundingUp(a, b, m *big.Int) (*big.Int, error) {
	if err := checkMulDivOperands(a, b, m); err != nil {
		return nil, err
	}
	product := new(big.Int).Mul(a, b)
	var remainder big.Int
	quotient, _ := product.QuoRem(product, m, &remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, one)
	}
	return quotient, nil
}

// divRoundingUp returns ceil(a/b) for a non-negative and b positive.
func divRoundingUp(a, b *big.Int) (*big.Int, error) {
	if err := checkNonNegative("dividend", a); err != nil {
		return nil, err
	}
	if err := checkPositive("divisor", b); err != nil {
		return nil, err
	}
	var remainder big.Int
	quotient, _ := new(big.Int).QuoRem(a, b, &remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, one)
	}
	return quotient, nil
}

func checkMulDivOperands(a, b, m *big.Int) error {
	if a == nil || b == nil || m == nil {
		return fmt.Errorf("%w: nil operand", ErrInvalidArgument)
	}
	if a.Sign() < 0 || b.Sign() < 0 || m.Sign() < 0 {
		return fmt.Errorf("%w: negative operand", ErrInvalidArgument)
	}
	if m.Sign() == 0 {
		return fmt.Errorf("%w: zero modulus", ErrInvalidArgument)
	}
	return nil
}
