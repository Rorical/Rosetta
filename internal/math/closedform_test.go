package math

import (
	"errors"
	"math/big"
	"testing"
)

func TestClosedFormTwoLegMatchesBruteForce(t *testing.T) {
	tests := []struct {
		name string
		leg1 ClosedFormLeg
		leg2 ClosedFormLeg
	}{
		{
			name: "small profitable",
			leg1: ClosedFormLeg{
				InReserve:  big.NewInt(1_000_000),
				OutReserve: big.NewInt(1_100_000),
				FeePips:    3000,
			},
			leg2: ClosedFormLeg{
				InReserve:  big.NewInt(1_000_000),
				OutReserve: big.NewInt(1_000_000),
				FeePips:    3000,
			},
		},
		{
			name: "zero fees",
			leg1: ClosedFormLeg{
				InReserve:  big.NewInt(1_000_000),
				OutReserve: big.NewInt(1_100_000),
			},
			leg2: ClosedFormLeg{
				InReserve:  big.NewInt(1_000_000),
				OutReserve: big.NewInt(1_000_000),
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			dStar, gross, net, profitable, err := ClosedFormTwoLeg(test.leg1, test.leg2)
			if err != nil {
				t.Fatalf("ClosedFormTwoLeg: %v", err)
			}
			if !profitable {
				t.Fatalf("expected a profitable cycle")
			}
			if dStar.Sign() <= 0 || gross.Cmp(dStar) <= 0 || net.Sign() <= 0 {
				t.Fatalf("inconsistent result d=%s gross=%s net=%s", dStar, gross, net)
			}
			if got := new(big.Int).Sub(gross, dStar); got.Cmp(net) != 0 {
				t.Fatalf("net = %s, want gross-d = %s", net, got)
			}

			// The returned gross is the floor of the exact rational formula at
			// d*, so it can never exceed the real-valued model.
			realGross := analyticGrossRat(test.leg1, test.leg2, dStar.Int64())
			if floor := ratFloor(realGross); gross.Cmp(floor) != 0 {
				t.Fatalf("gross = %s, want floor of %s = %s", gross, realGross.FloatString(6), floor)
			}
			if new(big.Rat).SetInt(gross).Cmp(realGross) > 0 {
				t.Fatalf("gross %s overestimates %s", gross, realGross.FloatString(6))
			}

			limit := 2*dStar.Int64() + 200
			bestNet := int64(0)
			for d := int64(0); d <= limit; d++ {
				value := ratFloor(analyticGrossRat(test.leg1, test.leg2, d))
				candidate := new(big.Int).Sub(value, big.NewInt(d))
				if candidate.IsInt64() && candidate.Int64() > bestNet {
					bestNet = candidate.Int64()
				}
			}
			if !net.IsInt64() || net.Int64() != bestNet {
				t.Fatalf("net = %s, brute-force best = %d", net, bestNet)
			}
		})
	}
}

func TestClosedFormTwoLegUnprofitable(t *testing.T) {
	leg := ClosedFormLeg{
		InReserve:  big.NewInt(1_000_000),
		OutReserve: big.NewInt(1_000_000),
		FeePips:    3000,
	}
	dStar, gross, net, profitable, err := ClosedFormTwoLeg(leg, leg)
	if err != nil {
		t.Fatalf("ClosedFormTwoLeg: %v", err)
	}
	if profitable {
		t.Fatalf("expected unprofitable cycle")
	}
	if dStar.Sign() != 0 || gross.Sign() != 0 || net.Sign() != 0 {
		t.Fatalf("unprofitable result = (%s, %s, %s), want zeros", dStar, gross, net)
	}
}

func TestClosedFormTwoLegErrors(t *testing.T) {
	valid := ClosedFormLeg{
		InReserve:  big.NewInt(1_000_000),
		OutReserve: big.NewInt(1_000_000),
		FeePips:    3000,
	}
	tests := []struct {
		name string
		leg1 ClosedFormLeg
		leg2 ClosedFormLeg
	}{
		{"nil in reserve", ClosedFormLeg{OutReserve: big.NewInt(1)}, valid},
		{"nil out reserve", ClosedFormLeg{InReserve: big.NewInt(1)}, valid},
		{"zero in reserve", ClosedFormLeg{InReserve: new(big.Int), OutReserve: big.NewInt(1)}, valid},
		{"negative out reserve", ClosedFormLeg{InReserve: big.NewInt(1), OutReserve: big.NewInt(-1)}, valid},
		{"full fee", ClosedFormLeg{InReserve: big.NewInt(1), OutReserve: big.NewInt(1), FeePips: feePipsDenominator}, valid},
		{"bad second leg", valid, ClosedFormLeg{InReserve: big.NewInt(1), OutReserve: big.NewInt(1), FeePips: feePipsDenominator + 1}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, _, _, _, err := ClosedFormTwoLeg(test.leg1, test.leg2); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func analyticGrossRat(leg1, leg2 ClosedFormLeg, d int64) *big.Rat {
	c1 := feeComplementRat(leg1.FeePips)
	c2 := feeComplementRat(leg2.FeePips)

	netIn1 := new(big.Rat).Mul(new(big.Rat).SetInt64(d), c1)
	denominator1 := new(big.Rat).Add(new(big.Rat).SetInt(leg1.InReserve), netIn1)
	middle := new(big.Rat).Mul(new(big.Rat).SetInt(leg1.OutReserve), netIn1)
	middle.Quo(middle, denominator1)

	netIn2 := new(big.Rat).Mul(middle, c2)
	denominator2 := new(big.Rat).Add(new(big.Rat).SetInt(leg2.InReserve), netIn2)
	output := new(big.Rat).Mul(new(big.Rat).SetInt(leg2.OutReserve), netIn2)
	return output.Quo(output, denominator2)
}

func feeComplementRat(feePips uint32) *big.Rat {
	return new(big.Rat).SetFrac(
		big.NewInt(int64(feePipsDenominator-feePips)),
		big.NewInt(int64(feePipsDenominator)),
	)
}
