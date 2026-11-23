package arb

import (
	"math"
	"math/big"
	"math/rand"
	"testing"

	poolmath "github.com/Rorical/Rosetta/internal/math"
)

func TestModelProfitUpperEnclosesIntegerEstimate(t *testing.T) {
	random := rand.New(rand.NewSource(510))
	for _, depth := range []int{2, 3, 6, 16} {
		for sample := range 200 {
			legs := make([]poolmath.ClosedFormLeg, depth)
			bounds := make([]modelInterval, depth)
			for i := range legs {
				shift := uint(random.Intn(250))
				in := new(big.Int).Lsh(big.NewInt(int64(10+random.Intn(100000))), shift)
				out := new(big.Int).Mul(in, big.NewInt(int64(95+random.Intn(16))))
				out.Quo(out, big.NewInt(100))
				if sample == 0 {
					if i < depth/2 {
						in.Lsh(in, 2000)
					} else {
						out.Lsh(out, 2000)
					}
				}
				legs[i] = poolmath.ClosedFormLeg{InReserve: in, OutReserve: out, FeePips: uint32(random.Intn(10000))}
				bounds[i] = newModelInterval(legs[i])
			}
			minimum := big.NewInt(1)
			maximum := new(big.Int).Lsh(big.NewInt(100000), uint(sample%250))
			for offset := range depth {
				rotated := append(append([]poolmath.ClosedFormLeg{}, legs[offset:]...), legs[:offset]...)
				_, net, err := poolmath.EstimateCycle(rotated, minimum, maximum)
				if err != nil {
					t.Fatal(err)
				}
				minLo, _ := integerInterval(minimum)
				_, maxHi := integerInterval(maximum)
				upper := modelCycleUpper(bounds, offset, minLo, maxHi)
				if math.IsInf(upper, 1) {
					continue
				}
				var ceiling big.Int
				ceilFloat(&ceiling, upper)
				if net.Cmp(&ceiling) > 0 {
					t.Fatalf("upper underestimates model: depth=%d sample=%d offset=%d upper=%s net=%s", depth, sample, offset, &ceiling, net)
				}
			}
		}
	}
}

func TestModelProfitUpperRejectsUnfundableMinimumWithoutSquareRoot(t *testing.T) {
	legs := []modelInterval{
		newModelInterval(poolmath.ClosedFormLeg{InReserve: big.NewInt(1000), OutReserve: big.NewInt(1100)}),
		newModelInterval(poolmath.ClosedFormLeg{InReserve: big.NewInt(1000), OutReserve: big.NewInt(1100)}),
	}
	if got := modelCycleUpper(legs, 0, 1000, 10000); got != 0 {
		t.Fatal("minimum already exceeds profitable range", got)
	}
	for _, x := range []float64{math.SmallestNonzeroFloat64, 0.5, 1, 1.01, math.Nextafter(1<<53, math.Inf(1)), math.MaxFloat64} {
		var got big.Int
		ceilFloat(&got, x)
		rational := new(big.Rat).SetFloat64(x)
		want, rem := new(big.Int), new(big.Int)
		want.QuoRem(rational.Num(), rational.Denom(), rem)
		if rem.Sign() != 0 {
			want.Add(want, big.NewInt(1))
		}
		if got.Cmp(want) != 0 {
			t.Fatal("incorrect integer ceiling", x, &got, want)
		}
	}
}

// The bit-stepping rounders must agree with math.Nextafter on every input,
// or ranking ceilings and therefore pruning would change.
func TestRoundingMatchesNextafter(t *testing.T) {
	values := []float64{0, math.Copysign(0, -1), math.SmallestNonzeroFloat64, -math.SmallestNonzeroFloat64,
		math.MaxFloat64, -math.MaxFloat64, math.Inf(1), math.Inf(-1), math.NaN(), 1, -1, 0x1p-1022, -0x1p-1022}
	random := rand.New(rand.NewSource(7))
	for range 200000 {
		values = append(values, math.Float64frombits(random.Uint64()))
	}
	for _, value := range values {
		down, up := max(0, math.Nextafter(value, math.Inf(-1))), math.Nextafter(value, math.Inf(1))
		if math.Float64bits(roundDown(value)) != math.Float64bits(down) || math.Float64bits(roundUp(value)) != math.Float64bits(up) {
			t.Fatalf("%v (%#x): down %v want %v, up %v want %v", value, math.Float64bits(value), roundDown(value), down, roundUp(value), up)
		}
	}
}
