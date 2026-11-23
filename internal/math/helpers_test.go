package math

import (
	"math/big"
	"testing"
)

func bigFromString(t *testing.T, s string) *big.Int {
	t.Helper()
	value, ok := new(big.Int).SetString(s, 10)
	if !ok {
		t.Fatalf("invalid decimal big.Int %q", s)
	}
	return value
}

func sqrtAt(t *testing.T, tick int32) *big.Int {
	t.Helper()
	price, err := TickToSqrtPriceX96(tick)
	if err != nil {
		t.Fatalf("TickToSqrtPriceX96(%d): %v", tick, err)
	}
	return price
}

func ratFloor(value *big.Rat) *big.Int {
	return new(big.Int).Quo(value.Num(), value.Denom())
}

func ratCeil(value *big.Rat) *big.Int {
	numerator, denominator := value.Num(), value.Denom()
	quotient := new(big.Int).Quo(numerator, denominator)
	if new(big.Int).Rem(numerator, denominator).Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	return quotient
}

func ratAbs(value *big.Rat) *big.Rat {
	return new(big.Rat).Abs(value)
}

func wordWithBits(bits ...uint) *big.Int {
	word := new(big.Int)
	for _, bit := range bits {
		word.SetBit(word, int(bit), 1)
	}
	return word
}
