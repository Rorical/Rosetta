package arb

import (
	"math"
	"math/big"
	"math/rand"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	poolmath "github.com/Rorical/Rosetta/internal/math"
)

func TestRoundedRateUpperAlwaysEnclosesExactRatio(t *testing.T) {
	random := rand.New(rand.NewSource(391))
	for _, bits := range []uint{1, 53, 256, 2000} {
		for range 50 {
			in := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), bits), big.NewInt(random.Int63n(10000)))
			out := new(big.Int).Add(new(big.Int).Lsh(big.NewInt(1), bits/2), big.NewInt(random.Int63n(10000)))
			for direction := range 2 {
				if direction == 1 {
					in, out = out, in
				}
				leg := poolmath.ClosedFormLeg{InReserve: in, OutReserve: out, FeePips: uint32(random.Intn(1_000_000))}
				upper := marginalRateUpper(leg)
				if upper <= 0 || math.IsNaN(upper) {
					t.Fatal("invalid upper bound", upper)
				}
				if math.IsInf(upper, 1) {
					continue
				}
				exact := new(big.Rat).SetFrac(new(big.Int).Mul(out, big.NewInt(int64(1_000_000-leg.FeePips))), new(big.Int).Mul(in, big.NewInt(1_000_000)))
				if new(big.Rat).SetFloat64(upper).Cmp(exact) < 0 {
					t.Fatal("rounded rate fell below exact rate", upper, exact)
				}
			}
		}
	}
}

func TestIntervalPruningMatchesIntegerBoundForLongAndExtremeCycles(t *testing.T) {
	random := rand.New(rand.NewSource(392))
	for _, depth := range []int{2, 6, 16} {
		for sample := range 100 {
			snapshot := &rankSnapshot{pools: make(map[domain.PoolKey]*rankPool)}
			cycle := graph.Cycle{Start: testAddress(1), Len: depth}
			legs := make([]poolmath.ClosedFormLeg, depth)
			for i := range legs {
				bits := uint(30 + random.Intn(227))
				in := new(big.Int).Lsh(big.NewInt(1), bits)
				out := new(big.Int).Add(new(big.Int).Set(in), big.NewInt(int64(random.Intn(3)-1)))
				fee := uint32(0)
				if sample%3 == 0 {
					fee = 3000
				}
				// Force both overflow and underflow in intermediate floating products.
				if sample == 0 {
					if i < depth/2 {
						out.Lsh(out, 2000)
					} else {
						in.Lsh(in, 2000)
					}
				}
				legs[i] = poolmath.ClosedFormLeg{InReserve: in, OutReserve: out, FeePips: fee}
				p := testV2State(byte(i+1), byte(i+1), byte((i+1)%depth+1), 1, 1)
				snapshot.pools[p.Key] = &rankPool{state: p, ok: true, legs: [2]rankLeg{{model: legs[i], rateUpper: marginalRateUpper(legs[i])}}}
				cycle.Pools = append(cycle.Pools, p.Key)
			}
			want, err := poolmath.CycleCanProfit(legs)
			if err != nil {
				t.Fatal(err)
			}
			work := rankWorkspaces.Get().(*rankWorkspace)
			got := work.cycleCanProfit(snapshot, cycle)
			work.release()
			if got != want {
				t.Fatal("interval pruning changed exact admission", depth, sample, got, want)
			}
		}
	}
}
