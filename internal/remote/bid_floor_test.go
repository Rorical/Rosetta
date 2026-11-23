package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/planning"
)

// On a two-hop route worth 1.2e7 wei per gas the window model can choose a
// tip of 1.3e5 at a predicted win of 0.36 while a rival takes the same route
// and the same gross in the same round with 2.7e6. Rivals on such pools pay
// 9–39% of an opportunity's worth; a bid far under what recent contests on
// the route's pools paid loses for certain.
func TestTipIsNotBelowWhatRecentContestsOnTheRoutePaid(t *testing.T) {
	h := newBidHistory()
	// Enough quiet windows elsewhere for the model to apply at all.
	h.quiet(elsewhere, 12_000_000, 60)
	// Three contests on the route at 20%, 25% and 30% of each window's worth.
	h.window(route, 10_000_000, bidContest{from: rival2905, tip: 2_000_000})
	h.window(route, 20_000_000, bidContest{from: rival2905, tip: 5_000_000})
	h.window(route, 8_000_000, bidContest{from: rivalD4AF, tip: 2_400_000})
	d := h.decide(route, 12_000_000)
	if d == nil || d.ModelTip == nil {
		t.Fatalf("no decision: %+v", d)
	}
	if got := d.ModelTip.Uint64(); got < 3_000_000 {
		t.Fatalf("tip %d is under a quarter of the worth, the median of the route's contests", got)
	}
	if d.ContestFloor != 3_000_000 {
		t.Fatalf("contest floor %d", d.ContestFloor)
	}

	// Two contests say too little, and contests on other pools say nothing.
	fresh := newBidHistory()
	fresh.quiet(elsewhere, 12_000_000, 60)
	fresh.window(route, 10_000_000, bidContest{from: rival2905, tip: 2_000_000})
	fresh.window(route, 10_000_000, bidContest{from: rival2905, tip: 2_000_000})
	fresh.window(unrelated, 10_000_000, bidContest{from: rival2905, tip: 9_000_000})
	if d := fresh.decide(route, 12_000_000); d == nil || d.ContestFloor != 0 {
		t.Fatalf("floor from two contests: %+v", d)
	}

	// Contests that paid more than half the worth are not followed: the floor
	// stops at half, and the model decides above it.
	dear := newBidHistory()
	dear.quiet(elsewhere, 12_000_000, 60)
	for range 3 {
		dear.window(route, 10_000_000, bidContest{from: rival2905, tip: 9_000_000})
	}
	if d := dear.decide(route, 12_000_000); d == nil || d.ContestFloor != 6_000_000 {
		t.Fatalf("floor above half the worth: %+v", d)
	}
}

// In contests on the same pools rivals' tips typically take a median 39% of
// the gross; in replay, bidding 30% of the surplus keeps the most. When every
// pool is held a lost race costs nothing, so there is no reason to bid under
// that share: a large opportunity sent at a near-zero tip is simply dropped.
func TestAFreeLossBidsAtLeastThirtyPercentOfTheSurplus(t *testing.T) {
	h := newBidHistory()
	h.quiet(route, 12_000_000, 60) // the window model sees no competition and bids low
	r := &reactiveRuntime{bids: h.e}
	r.settings.document.BidMinSamples = 50
	r.settings.document.BidModel = "apply"
	r.settings.minWin = testMinWin
	request := func(loss uint64, conditional bool, ceiling int64) planning.BidRequest {
		surplus := new(big.Int).SetUint64(12_000_000 * 300_000)
		return planning.BidRequest{Route: route, Surplus: surplus, Gas: testGasLimit, Base: big.NewInt(300), Extra: big.NewInt(4_000_000_000), Ceiling: big.NewInt(ceiling), LossGas: loss, Conditional: conditional}
	}
	paid := r.priceBid(h.at, request(0, false, 7_200_000))
	if paid == nil || tipOf(paid) >= 3_600_000 {
		t.Fatalf("a send whose loss is paid for was floored: %+v", paid)
	}
	free := r.priceBid(h.at, request(planning.FreeLoss, true, 7_200_000))
	if free == nil || free.Reason != "" || tipOf(free) < 3_600_000 {
		t.Fatalf("a free loss bid %d, under 30%% of a worth of 12,000,000", tipOf(free))
	}
	// The ceiling still binds.
	if capped := r.priceBid(h.at, request(planning.FreeLoss, true, 1_000_000)); capped == nil || tipOf(capped) != 1_000_000 {
		t.Fatalf("the floor ignored the ceiling: %+v", capped)
	}
}

// On opportunities where a rival filled the same route, a rival's tip
// typically takes a median 73% of a surplus under $0.02 and 34% of one between
// $0.10 and $0.30. In replay, bidding 50% under $0.05, 40% up to $0.30 and 30%
// above wins about 40% more races than a flat 30% and keeps slightly more.
func TestTheFreeLossShareFallsAsTheSurplusGrows(t *testing.T) {
	for _, c := range []struct {
		surplus  int64 // wei
		num, den uint64
	}{
		{3_600_000_000_000, 5, 10},     // $0.007
		{24_999_999_999_999, 5, 10},    // just under the first step
		{25_000_000_000_000, 4, 10},    // $0.05
		{149_999_999_999_999, 4, 10},   // just under the second step
		{150_000_000_000_000, 3, 10},   // $0.31
		{5_000_000_000_000_000, 3, 10}, // $10
	} {
		if num, den := freeLossShare(big.NewInt(c.surplus)); num != c.num || den != c.den {
			t.Errorf("surplus %d: share %d/%d, want %d/%d", c.surplus, num, den, c.num, c.den)
		}
	}
}
