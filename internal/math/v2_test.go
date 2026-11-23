package math

import (
	"errors"
	"math/big"
	"testing"
)

func TestQuoteV2ExactIn(t *testing.T) {
	tests := []struct {
		name       string
		reserveIn  string
		reserveOut string
		feePips    uint32
		amountIn   string
	}{
		{"constant product", "1000", "1000", 0, "100"},
		{"constant product large", "1000000000", "2000000000", 0, "100000000"},
		{"thousandth fee", "1000000", "2000000", 1000, "10000"},
		{"thirty bps fee", "1000000000000000000", "2000000000000000000", 3000, "100000000000000000"},
		{"tiny input", "1000000", "1000000", 3000, "1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reserveIn := bigFromString(t, test.reserveIn)
			reserveOut := bigFromString(t, test.reserveOut)
			amountIn := bigFromString(t, test.amountIn)

			got, err := QuoteV2ExactIn(reserveIn, reserveOut, test.feePips, amountIn)
			if err != nil {
				t.Fatalf("QuoteV2ExactIn: %v", err)
			}
			want := expectedV2Out(reserveIn, reserveOut, test.feePips, amountIn)
			if got.Cmp(want) != 0 {
				t.Fatalf("QuoteV2ExactIn = %s, want %s", got, want)
			}
			if got.Cmp(reserveOut) >= 0 {
				t.Fatalf("output %s reached reserve %s", got, reserveOut)
			}
		})
	}
}

func TestQuoteV2KnownConstantProduct(t *testing.T) {
	reserveIn := big.NewInt(1_000_000_000)
	reserveOut := big.NewInt(2_000_000_000)
	amountIn := big.NewInt(100_000_000)
	got, err := QuoteV2ExactIn(reserveIn, reserveOut, 0, amountIn)
	if err != nil {
		t.Fatalf("QuoteV2ExactIn: %v", err)
	}
	want := big.NewInt(181_818_181)
	if got.Cmp(want) != 0 {
		t.Fatalf("output = %s, want %s", got, want)
	}
}

func TestQuoteV2Errors(t *testing.T) {
	tests := []struct {
		name       string
		reserveIn  *big.Int
		reserveOut *big.Int
		feePips    uint32
		amountIn   *big.Int
	}{
		{"nil reserve in", nil, big.NewInt(1), 3000, big.NewInt(1)},
		{"nil reserve out", big.NewInt(1), nil, 3000, big.NewInt(1)},
		{"nil amount", big.NewInt(1), big.NewInt(1), 3000, nil},
		{"zero reserve", new(big.Int), big.NewInt(1), 3000, big.NewInt(1)},
		{"negative reserve", big.NewInt(-1), big.NewInt(1), 3000, big.NewInt(1)},
		{"zero amount", big.NewInt(1), big.NewInt(1), 3000, new(big.Int)},
		{"negative amount", big.NewInt(1), big.NewInt(1), 3000, big.NewInt(-1)},
		{"full fee", big.NewInt(1), big.NewInt(1), feePipsDenominator, big.NewInt(1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := QuoteV2ExactIn(test.reserveIn, test.reserveOut, test.feePips, test.amountIn); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func expectedV2Out(reserveIn, reserveOut *big.Int, feePips uint32, amountIn *big.Int) *big.Int {
	c := new(big.Rat).SetFrac(
		big.NewInt(int64(feePipsDenominator-feePips)),
		big.NewInt(int64(feePipsDenominator)),
	)
	effectiveIn := new(big.Rat).Mul(new(big.Rat).SetInt(amountIn), c)
	denominator := new(big.Rat).Add(new(big.Rat).SetInt(reserveIn), effectiveIn)
	numerator := new(big.Rat).Mul(new(big.Rat).SetInt(reserveOut), effectiveIn)
	return ratFloor(new(big.Rat).Quo(numerator, denominator))
}

func TestQuoteV2ExactOut(t *testing.T) {
	tests := []struct {
		name       string
		reserveIn  string
		reserveOut string
		feePips    uint32
		amountOut  string
	}{
		{"constant product", "1000", "1000", 0, "100"},
		{"constant product large", "1000000000", "2000000000", 0, "100000000"},
		{"thousandth fee", "1000000", "2000000", 1000, "10000"},
		{"thirty bps fee", "1000000000000000000", "2000000000000000000", 3000, "100000000000000000"},
		{"tiny output", "1000000", "1000000", 3000, "1"},
		{"most of the reserve", "1000000", "2000000", 3000, "1999999"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			reserveIn := bigFromString(t, test.reserveIn)
			reserveOut := bigFromString(t, test.reserveOut)
			amountOut := bigFromString(t, test.amountOut)

			got, err := QuoteV2ExactOut(reserveIn, reserveOut, test.feePips, amountOut)
			if err != nil {
				t.Fatalf("QuoteV2ExactOut: %v", err)
			}
			want := expectedV2In(reserveIn, reserveOut, test.feePips, amountOut)
			if got.Cmp(want) != 0 {
				t.Fatalf("QuoteV2ExactOut = %s, want %s", got, want)
			}
			if got.Sign() <= 0 {
				t.Fatalf("input %s is not positive", got)
			}
		})
	}
}

func TestQuoteV2ExactOutProducesRequestedOutput(t *testing.T) {
	tests := []struct {
		reserveIn  string
		reserveOut string
		feePips    uint32
		amountOut  string
	}{
		{"1000", "1000", 0, "100"},
		{"1000000000", "2000000000", 0, "100000000"},
		{"1000000", "2000000", 1000, "10000"},
		{"1000000000000000000", "2000000000000000000", 3000, "100000000000000000"},
		{"1000000", "1000000", 3000, "1"},
		{"1000000", "2000000", 3000, "1999999"},
	}
	for _, test := range tests {
		reserveIn := bigFromString(t, test.reserveIn)
		reserveOut := bigFromString(t, test.reserveOut)
		amountOut := bigFromString(t, test.amountOut)

		amountIn, err := QuoteV2ExactOut(reserveIn, reserveOut, test.feePips, amountOut)
		if err != nil {
			t.Fatalf("QuoteV2ExactOut(%s, %s, %d, %s): %v",
				test.reserveIn, test.reserveOut, test.feePips, test.amountOut, err)
		}
		delivered, err := QuoteV2ExactIn(reserveIn, reserveOut, test.feePips, amountIn)
		if err != nil {
			t.Fatalf("QuoteV2ExactIn: %v", err)
		}
		if delivered.Cmp(amountOut) < 0 {
			t.Fatalf("input %s delivers %s, want at least %s", amountIn, delivered, amountOut)
		}
	}
}

func TestQuoteV2ExactOutRoundTripsExactIn(t *testing.T) {
	reserveIn := big.NewInt(1_000_000_000)
	reserveOut := big.NewInt(2_000_000_000)
	for _, feePips := range []uint32{0, 500, 3000, 10_000} {
		for _, amountIn := range []*big.Int{
			big.NewInt(1),
			big.NewInt(1_000_000),
			big.NewInt(100_000_000),
		} {
			amountOut, err := QuoteV2ExactIn(reserveIn, reserveOut, feePips, amountIn)
			if err != nil {
				t.Fatalf("QuoteV2ExactIn: %v", err)
			}
			needed, err := QuoteV2ExactOut(reserveIn, reserveOut, feePips, amountOut)
			if err != nil {
				t.Fatalf("QuoteV2ExactOut: %v", err)
			}
			if needed.Cmp(amountIn) > 0 {
				t.Fatalf("fee %d: output %s of input %s needs %s to recreate",
					feePips, amountOut, amountIn, needed)
			}
		}
	}
}

func TestQuoteV2ExactOutErrors(t *testing.T) {
	tests := []struct {
		name       string
		reserveIn  *big.Int
		reserveOut *big.Int
		feePips    uint32
		amountOut  *big.Int
	}{
		{"nil reserve in", nil, big.NewInt(1), 3000, big.NewInt(1)},
		{"nil reserve out", big.NewInt(1), nil, 3000, big.NewInt(1)},
		{"nil amount", big.NewInt(1), big.NewInt(10), 3000, nil},
		{"zero reserve", new(big.Int), big.NewInt(1), 3000, big.NewInt(1)},
		{"negative reserve", big.NewInt(-1), big.NewInt(10), 3000, big.NewInt(1)},
		{"zero amount", big.NewInt(1), big.NewInt(10), 3000, new(big.Int)},
		{"negative amount", big.NewInt(1), big.NewInt(10), 3000, big.NewInt(-1)},
		{"full fee", big.NewInt(1), big.NewInt(10), feePipsDenominator, big.NewInt(1)},
		{"fee above denominator", big.NewInt(1), big.NewInt(10), feePipsDenominator + 1, big.NewInt(1)},
		{"amount equals reserve", big.NewInt(1), big.NewInt(10), 3000, big.NewInt(10)},
		{"amount above reserve", big.NewInt(1), big.NewInt(10), 3000, big.NewInt(11)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := QuoteV2ExactOut(test.reserveIn, test.reserveOut, test.feePips, test.amountOut); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func expectedV2In(reserveIn, reserveOut *big.Int, feePips uint32, amountOut *big.Int) *big.Int {
	numerator := new(big.Rat).Mul(new(big.Rat).SetInt(reserveIn), new(big.Rat).SetInt(amountOut))
	remaining := new(big.Rat).Sub(new(big.Rat).SetInt(reserveOut), new(big.Rat).SetInt(amountOut))
	feeComplement := new(big.Rat).SetFrac(
		big.NewInt(int64(feePipsDenominator-feePips)),
		big.NewInt(int64(feePipsDenominator)),
	)
	denominator := new(big.Rat).Mul(remaining, feeComplement)
	return ratCeil(new(big.Rat).Quo(numerator, denominator))
}
