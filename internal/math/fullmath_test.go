package math

import (
	"errors"
	"math/big"
	"testing"
)

func TestMulDiv(t *testing.T) {
	tests := []struct {
		name    string
		a, b, m string
		want    string
	}{
		{"simple", "10", "10", "3", "33"},
		{"exact", "10", "10", "5", "20"},
		{"zero product", "0", "12345", "7", "0"},
		{"512 bit product", "1461501637330902918203684832716283019655932542976", "1267650600228229401496703205376", "1267650600228229401496703205376", "1461501637330902918203684832716283019655932542976"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			a := bigFromString(t, test.a)
			b := bigFromString(t, test.b)
			m := bigFromString(t, test.m)
			aBefore := new(big.Int).Set(a)
			bBefore := new(big.Int).Set(b)
			mBefore := new(big.Int).Set(m)

			got, err := MulDiv(a, b, m)
			if err != nil {
				t.Fatalf("MulDiv: %v", err)
			}
			if want := bigFromString(t, test.want); got.Cmp(want) != 0 {
				t.Fatalf("MulDiv = %s, want %s", got, want)
			}
			if a.Cmp(aBefore) != 0 || b.Cmp(bBefore) != 0 || m.Cmp(mBefore) != 0 {
				t.Fatalf("MulDiv mutated its inputs")
			}
		})
	}
}

func TestMulDivErrors(t *testing.T) {
	tests := []struct {
		name    string
		a, b, m *big.Int
	}{
		{"nil", nil, big.NewInt(1), big.NewInt(1)},
		{"zero modulus", big.NewInt(1), big.NewInt(1), new(big.Int)},
		{"negative a", big.NewInt(-1), big.NewInt(1), big.NewInt(1)},
		{"negative b", big.NewInt(1), big.NewInt(-1), big.NewInt(1)},
		{"negative modulus", big.NewInt(1), big.NewInt(1), big.NewInt(-1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := MulDiv(test.a, test.b, test.m); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("MulDiv error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func TestRoundingHelpers(t *testing.T) {
	up, err := mulDivRoundingUp(big.NewInt(10), big.NewInt(10), big.NewInt(3))
	if err != nil {
		t.Fatalf("mulDivRoundingUp: %v", err)
	}
	if up.Int64() != 34 {
		t.Fatalf("mulDivRoundingUp(10,10,3) = %d, want 34", up)
	}
	exact, err := mulDivRoundingUp(big.NewInt(10), big.NewInt(10), big.NewInt(5))
	if err != nil {
		t.Fatalf("mulDivRoundingUp exact: %v", err)
	}
	if exact.Int64() != 20 {
		t.Fatalf("mulDivRoundingUp(10,10,5) = %d, want 20", exact)
	}

	divUp, err := divRoundingUp(big.NewInt(10), big.NewInt(3))
	if err != nil {
		t.Fatalf("divRoundingUp: %v", err)
	}
	if divUp.Int64() != 4 {
		t.Fatalf("divRoundingUp(10,3) = %d, want 4", divUp)
	}
	if _, err := divRoundingUp(big.NewInt(10), new(big.Int)); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("divRoundingUp with zero divisor error = %v", err)
	}
}
