package math

import (
	"math/big"
	"math/rand"
	"sync"
	"testing"
)

func TestEstimateCycleLongRoutesAgainstRationalSearch(t *testing.T) {
	random := rand.New(rand.NewSource(17))
	for _, depth := range []int{2, 3, 8, 16} {
		for sample := range 8 {
			legs := make([]ClosedFormLeg, depth)
			for i := range legs {
				legs[i] = ClosedFormLeg{InReserve: big.NewInt(20 + random.Int63n(200)), OutReserve: big.NewInt(20 + random.Int63n(200)), FeePips: uint32(random.Intn(10000))}
			}
			// Include near-total fees and unprofitable continuous optima.
			if sample == 0 {
				legs[0].FeePips = 999999
			}
			minimum, maximum := int64(1+sample), int64(40+sample)
			input, net, err := EstimateCycle(legs, big.NewInt(minimum), big.NewInt(maximum))
			if err != nil {
				t.Fatal(err)
			}
			var best *big.Int
			for amount := minimum; amount <= maximum; amount++ {
				value := rationalCycleProfit(legs, amount)
				if best == nil || value.Cmp(best) > 0 {
					best = value
				}
			}
			if input.Int64() < minimum || input.Int64() > maximum || net.Cmp(best) != 0 || net.Cmp(rationalCycleProfit(legs, input.Int64())) != 0 {
				t.Fatalf("depth=%d input=%s net=%s exhaustive=%s", depth, input, net, best)
			}
		}
	}
}

func rationalCycleProfit(legs []ClosedFormLeg, amount int64) *big.Int {
	output := big.NewRat(amount, 1)
	for _, leg := range legs {
		afterFee := new(big.Rat).Mul(output, big.NewRat(int64(1_000_000-leg.FeePips), 1_000_000))
		numerator := new(big.Rat).Mul(afterFee, new(big.Rat).SetInt(leg.OutReserve))
		denominator := new(big.Rat).Add(new(big.Rat).SetInt(leg.InReserve), afterFee)
		output.Quo(numerator, denominator)
	}
	return new(big.Int).Sub(new(big.Int).Div(output.Num(), output.Denom()), big.NewInt(amount))
}

func TestEstimateCycleResultsSurviveReuseAndConcurrentCalls(t *testing.T) {
	legs := []ClosedFormLeg{
		{InReserve: big.NewInt(1_000_000), OutReserve: big.NewInt(2_000_000)},
		{InReserve: big.NewInt(1_000_000), OutReserve: big.NewInt(2_000_000)},
	}
	input, net, err := EstimateCycle(legs, big.NewInt(1), big.NewInt(1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 50 {
				got, _, err := EstimateCycle(legs, big.NewInt(1), big.NewInt(100))
				if err != nil || got.Int64() != 100 {
					t.Error(got, err)
					return
				}
				got.SetInt64(0)
			}
		})
	}
	workers.Wait()
	if input.Int64() != 333333 || net.Int64() != 333333 || legs[0].InReserve.Int64() != 1_000_000 || legs[1].OutReserve.Int64() != 2_000_000 {
		t.Fatal("pooled words escaped into inputs or previous results")
	}
}

func TestEstimateCycleWideReserves(t *testing.T) {
	for _, depth := range []int{2, 8, 16} {
		legs := make([]ClosedFormLeg, depth)
		for i := range legs {
			reserve := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), 240), big.NewInt(int64(i+1)))
			legs[i] = ClosedFormLeg{InReserve: reserve, OutReserve: new(big.Int).Lsh(reserve, 1), FeePips: 3000}
		}
		maximum := new(big.Int).Lsh(big.NewInt(1), 128)
		input, profit, err := EstimateCycle(legs, big.NewInt(1), maximum)
		if err != nil || input.Cmp(maximum) != 0 {
			t.Fatal(input, err)
		}
		output := new(big.Rat).SetInt(input)
		for _, leg := range legs {
			afterFee := new(big.Rat).Mul(output, big.NewRat(997, 1000))
			output.Quo(new(big.Rat).Mul(afterFee, new(big.Rat).SetInt(leg.OutReserve)), new(big.Rat).Add(new(big.Rat).SetInt(leg.InReserve), afterFee))
		}
		want := new(big.Int).Sub(new(big.Int).Div(output.Num(), output.Denom()), input)
		if profit.Cmp(want) != 0 {
			t.Fatal("wide reserve composition changed", profit, want)
		}
	}
}
