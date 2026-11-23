package arb

import (
	stdmath "math"
	"math/big"
	"math/rand/v2"
	"testing"

	"github.com/Rorical/Rosetta/internal/math"
)

// The float bounds settle a route only where CycleCanProfit would agree:
// random cycles, and break-even ones where the integer product decides.
func TestMarginProfitableAgreesWithTheIntegerProduct(t *testing.T) {
	random := rand.New(rand.NewPCG(7, 11))
	reserve := func() *big.Int {
		r := new(big.Int).Lsh(big.NewInt(int64(random.Uint32()|1)), uint(random.IntN(120)))
		return r.Add(r, big.NewInt(int64(random.IntN(1000))))
	}
	decided := 0
	for trial := range 20000 {
		n := 2 + random.IntN(4)
		legs := make([]math.ClosedFormLeg, n)
		for i := range legs {
			legs[i] = math.ClosedFormLeg{InReserve: reserve(), OutReserve: reserve(), FeePips: uint32(random.IntN(3) * 500)}
		}
		if trial%3 == 0 {
			// Close at break-even: the last leg trades back through the first pool.
			legs = []math.ClosedFormLeg{legs[0], {InReserve: legs[0].OutReserve, OutReserve: legs[0].InReserve}}
			legs[0].FeePips = 0
		}
		upper, lower := 1.0, 1.0
		for _, leg := range legs {
			interval := newModelInterval(leg)
			upper = stdmath.Nextafter(upper*interval.rateHigh, stdmath.Inf(1))
			lower = roundDown(lower * interval.rateLow)
		}
		want, err := math.CycleCanProfit(legs)
		if err != nil {
			t.Fatal(err)
		}
		if got := marginProfitable(upper, lower, legs); got != want {
			t.Fatalf("trial %d: bounds [%g, %g] gave %v, integer product %v", trial, lower, upper, got, want)
		}
		if trial%3 != 0 && (upper <= 1 || lower > 1) {
			decided++
		}
	}
	// Random reserves almost never land in the rounding band.
	if decided < 13300 {
		t.Fatal("float bounds settled too few random routes", decided)
	}
}
