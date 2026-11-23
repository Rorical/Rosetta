package math

import (
	"math/big"
	"testing"
)

func TestEstimateCycleMatchesIndependentRationalEnumeration(t *testing.T) {
	for _, fees := range [][]uint32{{0, 0, 0}, {3000, 500, 10000}, {999999, 0, 0}} {
		legs := []ClosedFormLeg{
			{InReserve: big.NewInt(1000), OutReserve: big.NewInt(1800), FeePips: fees[0]},
			{InReserve: big.NewInt(2000), OutReserve: big.NewInt(2300), FeePips: fees[1]},
			{InReserve: big.NewInt(1500), OutReserve: big.NewInt(1900), FeePips: fees[2]},
		}
		input, estimated, err := EstimateCycle(legs, big.NewInt(1), big.NewInt(1000))
		if err != nil {
			t.Fatal(err)
		}
		best := big.NewInt(-1 << 62)
		for amount := int64(1); amount <= 1000; amount++ {
			output := big.NewRat(amount, 1)
			for _, leg := range legs {
				afterFee := new(big.Rat).Mul(output, big.NewRat(int64(1_000_000-leg.FeePips), 1_000_000))
				output = new(big.Rat).Quo(new(big.Rat).Mul(afterFee, new(big.Rat).SetInt(leg.OutReserve)), new(big.Rat).Add(new(big.Rat).SetInt(leg.InReserve), afterFee))
			}
			profit := new(big.Int).Sub(new(big.Int).Div(output.Num(), output.Denom()), big.NewInt(amount))
			if profit.Cmp(best) > 0 {
				best.Set(profit)
			}
		}
		if estimated.Cmp(best) != 0 || input.Sign() <= 0 || input.Int64() > 1000 {
			t.Fatalf("fees=%v input=%s estimate=%s exhaustive=%s", fees, input, estimated, best)
		}
	}
}

func TestEstimateCycleTwoLegReferenceAndBounds(t *testing.T) {
	legs := []ClosedFormLeg{
		{InReserve: big.NewInt(1_000_000), OutReserve: big.NewInt(2_000_000)},
		{InReserve: big.NewInt(1_000_000), OutReserve: big.NewInt(2_000_000)},
	}
	input, net, err := EstimateCycle(legs, big.NewInt(1), big.NewInt(1_000_000))
	if err != nil || input.Int64() != 333333 || net.Int64() != 333333 {
		t.Fatalf("input=%v net=%v error=%v", input, net, err)
	}
	input, _, err = EstimateCycle(legs, big.NewInt(1), big.NewInt(100))
	if err != nil || input.Int64() != 100 {
		t.Fatalf("maximum not respected: %v %v", input, err)
	}
}
