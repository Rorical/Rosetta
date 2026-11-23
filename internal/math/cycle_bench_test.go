package math

import (
	"fmt"
	"math/big"
	"testing"
)

func BenchmarkEstimateCycle(b *testing.B) {
	for _, depth := range []int{2, 8, 16} {
		b.Run(fmt.Sprintf("%d-hop", depth), func(b *testing.B) {
			legs := make([]ClosedFormLeg, depth)
			for i := range legs {
				legs[i] = ClosedFormLeg{InReserve: big.NewInt(1_000_000_000), OutReserve: big.NewInt(1_050_000_000), FeePips: 3000}
			}
			minimum, maximum := big.NewInt(1), big.NewInt(1_000_000)
			b.ReportAllocs()
			for b.Loop() {
				input, net, err := EstimateCycle(legs, minimum, maximum)
				if err != nil || input.Sign() <= 0 || net.Sign() <= 0 {
					b.Fatal(input, net, err)
				}
			}
		})
	}
}

// Thin shallow edge, but minimum capital already consumes it through impact.
func BenchmarkEstimateCycleMinimumFunding(b *testing.B) {
	legs := make([]ClosedFormLeg, 6)
	for i := range legs {
		legs[i] = ClosedFormLeg{InReserve: big.NewInt(1000000000), OutReserve: big.NewInt(1050000000), FeePips: 3000}
	}
	minimum, maximum := big.NewInt(1000000000), big.NewInt(2000000000)
	input, net := new(big.Int), new(big.Int)
	b.ReportAllocs()
	for b.Loop() {
		if err := EstimateCycleInto(input, net, legs, minimum, maximum); err != nil || input.Cmp(minimum) != 0 || net.Sign() >= 0 {
			b.Fatal(input, net, err)
		}
	}
}
