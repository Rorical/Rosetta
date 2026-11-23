package math

import (
	"math/big"
	"math/rand"
	"testing"
)

func TestCycleSlopeBoundNeverRejectsPositiveRationalProfit(t *testing.T) {
	random := rand.New(rand.NewSource(81))
	for _, depth := range []int{2, 6, 16} {
		for range 100 {
			legs := make([]ClosedFormLeg, depth)
			slope := big.NewRat(1, 1)
			for i := range legs {
				legs[i] = ClosedFormLeg{InReserve: big.NewInt(1 + random.Int63n(2000)), OutReserve: big.NewInt(1 + random.Int63n(2000)), FeePips: uint32(random.Intn(1_000_000))}
				leg := legs[i]
				slope.Mul(slope, new(big.Rat).SetFrac(new(big.Int).Mul(leg.OutReserve, big.NewInt(int64(1_000_000-leg.FeePips))), new(big.Int).Mul(leg.InReserve, big.NewInt(1_000_000))))
			}
			possible, err := CycleCanProfit(legs)
			if err != nil || possible != (slope.Cmp(big.NewRat(1, 1)) > 0) {
				t.Fatal("slope bound changed", possible, slope, err)
			}
			var input, net big.Int
			if err := EstimateCycleInto(&input, &net, legs, big.NewInt(1), big.NewInt(50)); err != nil {
				t.Fatal(err)
			}
			var best *big.Int
			for amount := int64(1); amount <= 50; amount++ {
				profit := rationalCycleProfit(legs, amount)
				if best == nil || profit.Cmp(best) > 0 {
					best = profit
				}
			}
			if net.Cmp(best) != 0 || (!possible && net.Sign() > 0) {
				t.Fatal("pruning or scaled composition lost the optimum", net, best)
			}
		}
	}
}

func TestCycleSlopeIncludesFeesAndExactBreakEven(t *testing.T) {
	legs := []ClosedFormLeg{{InReserve: big.NewInt(1_000_000), OutReserve: big.NewInt(2_000_000)}, {InReserve: big.NewInt(2_000_000), OutReserve: big.NewInt(1_000_000)}}
	if possible, err := CycleCanProfit(legs); err != nil || possible {
		t.Fatal(possible, err)
	}
	legs[1].OutReserve.Add(legs[1].OutReserve, big.NewInt(1))
	if possible, err := CycleCanProfit(legs); err != nil || !possible {
		t.Fatal(possible, err)
	}
	legs[1].FeePips = 1
	if possible, err := CycleCanProfit(legs); err != nil || possible {
		t.Fatal(possible, err)
	}
	legs[1].FeePips = 1_000_000
	if _, err := CycleCanProfit(legs); err == nil {
		t.Fatal("invalid fee accepted")
	}
}
