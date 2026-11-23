package remote

import (
	"math/big"
	"testing"
)

func TestPaperBiddingChargesFeesOnceAndRespectsStrictPositiveBoundary(t *testing.T) {
	// Existing fee 40 includes gas=10 at tip=2. At zero tip, 80 remains.
	b := paperBids(big.NewInt(100), big.NewInt(40), big.NewInt(10), big.NewInt(2), big.NewInt(15), big.NewInt(5), []*big.Int{big.NewInt(7), big.NewInt(8), big.NewInt(11)})
	if b.ZeroTipNet.Int64() != 80 || b.MaxPositiveTip.Int64() != 7 {
		t.Fatal(b)
	}
	if !b.Scenarios[0].Positive || b.Scenarios[1].Positive || b.Scenarios[1].Net.Sign() != 0 || b.Scenarios[2].FitsFeeCap {
		t.Fatal(b.Scenarios)
	}
	// Cap, not arithmetic profitability, limits the admissible priority fee.
	capped := paperBids(big.NewInt(100), big.NewInt(40), big.NewInt(10), big.NewInt(2), big.NewInt(8), big.NewInt(5), nil)
	if capped.MaxPositiveTip.Int64() != 3 {
		t.Fatal(capped)
	}
	for _, profit := range []int64{0, 20} {
		lost := paperBids(big.NewInt(profit), big.NewInt(40), big.NewInt(10), big.NewInt(2), big.NewInt(15), big.NewInt(5), nil)
		if lost.Status != "non_positive" || lost.MaxPositiveTip != nil {
			t.Fatal("loss promoted to bid budget", lost)
		}
	}
	unknown := paperBids(nil, big.NewInt(40), big.NewInt(10), big.NewInt(2), big.NewInt(15), big.NewInt(5), nil)
	if unknown.Status != "unknown" || unknown.MaxPositiveTip != nil {
		t.Fatal(unknown)
	}
}
