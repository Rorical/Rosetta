package math

import (
	"math/big"
	"math/rand"
	"testing"
)

func TestCycleCoefficientsRotationsMatchIndependentComposition(t *testing.T) {
	random := rand.New(rand.NewSource(91))
	for _, depth := range []int{2, 3, 6, 16} {
		for sample := range 12 {
			legs := make([]ClosedFormLeg, depth)
			for i := range legs {
				in, out := big.NewInt(20+random.Int63n(200)), big.NewInt(20+random.Int63n(300))
				if sample == 0 {
					in.Lsh(in, 220)
					out.Lsh(out, 220)
				}
				legs[i] = ClosedFormLeg{InReserve: in, OutReserve: out, FeePips: uint32(random.Intn(10000))}
			}
			var estimator CycleEstimator
			var savedRoot *big.Int
			for rotation := range depth {
				rotated := append(append([]ClosedFormLeg{}, legs[rotation:]...), legs[:rotation]...)
				if err := estimator.Curve.SetLeg(rotated[0]); err != nil {
					t.Fatal(err)
				}
				for _, leg := range rotated[1:] {
					var suffix CycleCoefficients
					if err := suffix.SetLeg(leg); err != nil {
						t.Fatal(err)
					}
					estimator.Append(&suffix)
				}
				input, net := new(big.Int), new(big.Int)
				min, max := big.NewInt(int64(sample+1)), big.NewInt(int64(sample+40))
				if err := estimator.EstimateInto(input, net, min, max); err != nil {
					t.Fatal(err)
				}
				if estimator.root.Sign() > 0 {
					if savedRoot != nil && savedRoot.Cmp(&estimator.root) != 0 {
						t.Fatal("rotation changed invariant square root")
					}
					savedRoot = new(big.Int).Set(&estimator.root)
				}
				expectedInput, expectedNet, err := EstimateCycle(rotated, min, max)
				if err != nil || input.Cmp(expectedInput) != 0 || net.Cmp(expectedNet) != 0 {
					t.Fatalf("depth=%d sample=%d rotation=%d input=%s/%s net=%s/%s err=%v", depth, sample, rotation, input, expectedInput, net, expectedNet, err)
				}
				if sample > 0 {
					best := rationalCycleProfit(rotated, min.Int64())
					for amount := min.Int64() + 1; amount <= max.Int64(); amount++ {
						candidate := rationalCycleProfit(rotated, amount)
						if candidate.Cmp(best) > 0 {
							best = candidate
						}
					}
					if net.Cmp(best) != 0 {
						t.Fatal("rotation differs from independent rational search", net, best)
					}
				}
			}
		}
	}
}

func TestMinimumFundingRejectionAvoidsSquareRoot(t *testing.T) {
	legs := []ClosedFormLeg{{InReserve: big.NewInt(1000), OutReserve: big.NewInt(2000)}, {InReserve: big.NewInt(1000), OutReserve: big.NewInt(2000)}}
	var estimator CycleEstimator
	if err := estimator.Curve.SetLeg(legs[0]); err != nil {
		t.Fatal(err)
	}
	var suffix CycleCoefficients
	if err := suffix.SetLeg(legs[1]); err != nil {
		t.Fatal(err)
	}
	estimator.Append(&suffix)
	for _, minimum := range []int64{1000, 1001, 2000} {
		input, net := new(big.Int), new(big.Int)
		if err := estimator.EstimateInto(input, net, big.NewInt(minimum), big.NewInt(3000)); err != nil {
			t.Fatal(err)
		}
		if input.Int64() != minimum || net.Cmp(rationalCycleProfit(legs, minimum)) != 0 || net.Sign() > 0 || estimator.root.Sign() != 0 {
			t.Fatal("unfundable model computed square root or changed hint", input, net)
		}
	}
	input, net := new(big.Int), new(big.Int)
	if err := estimator.EstimateInto(input, net, big.NewInt(1), big.NewInt(3000)); err != nil || net.Sign() <= 0 || estimator.root.Sign() == 0 {
		t.Fatal(input, net, err)
	}
	oldRoot := new(big.Int).Set(&estimator.root)
	changed := legs[0]
	changed.OutReserve = big.NewInt(3000)
	if err := estimator.Curve.SetLeg(changed); err != nil {
		t.Fatal(err)
	}
	estimator.Append(&suffix)
	if err := estimator.EstimateInto(input, net, big.NewInt(1), big.NewInt(3000)); err != nil || estimator.root.Cmp(oldRoot) == 0 {
		t.Fatal("changed products reused stale square root", err)
	}
}
