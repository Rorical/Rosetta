package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
)

// bidHistory drives an estimator through admissions, rival sightings and
// closed windows at a fixed cadence, the way the feed and admissions do.
type bidHistory struct {
	e  *bidEstimator
	at time.Time
}

func newBidHistory() *bidHistory {
	return &bidHistory{e: newBidEstimator(250*time.Millisecond, 550*time.Millisecond), at: time.Unix(1_800_000_000, 0)}
}

// window opens one on pools priced at worth (0: never priced); each contest
// lands inside our flashblock.
func (h *bidHistory) window(pools []domain.PoolKey, worth uint64, contest ...bidContest) {
	h.e.register(pools, 0, h.at)
	if worth > 0 {
		h.e.mu.Lock()
		h.e.price(pools, worth, h.at.Add(5*time.Millisecond))
		h.e.mu.Unlock()
	}
	for _, c := range contest {
		h.e.observe(rivalSighting{pools: pools, from: c.from, to: c.to, tip: c.tip}, h.at.Add(300*time.Millisecond))
	}
	// Past bidContinue: each window is a separate opportunity.
	h.at = h.at.Add(bidContinue + time.Second)
}

func (h *bidHistory) quiet(pools []domain.PoolKey, worth uint64, n int) {
	for range n {
		h.window(pools, worth)
	}
}

const (
	testGasLimit = 500_000 // expected use 300,000 at the prior ratio
	testMinWin   = 0.2
)

// decide prices an opportunity worth wei per expected gas on route, with
// the surplus per unit of gas limit as the ceiling.
func (h *bidHistory) decide(route []domain.PoolKey, worth uint64) *execution.BidDecision {
	r := &reactiveRuntime{bids: h.e}
	r.settings.document.BidMinSamples = 50
	r.settings.document.BidModel = "apply"
	r.settings.minWin = testMinWin
	surplus := new(big.Int).SetUint64(worth * 300_000)
	ceiling := new(big.Int).Quo(surplus, big.NewInt(testGasLimit))
	return r.bidDecisionAt(h.at, route, surplus, testGasLimit, big.NewInt(300), big.NewInt(4_000_000_000), ceiling)
}

var (
	route     = []domain.PoolKey{{Address: domain.Address{1}}, {Address: domain.Address{2}}}
	elsewhere = []domain.PoolKey{{Address: domain.Address{7}}, {Address: domain.Address{8}}}
	unrelated = []domain.PoolKey{{Address: domain.Address{0x50}}}
	rival2905 = domain.Address{0x29, 0x05, 0xb5, 0x7c}
	rivalD4AF = domain.Address{0xd4, 0xaf, 0xb8, 0xc6}
	rivalA408 = domain.Address{0xa4, 0x08, 0x33, 0xc4}
	predator  = domain.Address{0xbb, 0xbb}
)

func tipOf(d *execution.BidDecision) uint64 { return d.ModelTip.Uint64() }

// An opportunity worth 12,255,993 wei per gas. A per-rival model bids about
// 56,000 against rival2905, which tips 7 wei on small windows and
// 12,224–521,441 on valuable ones, and predicts near-certain wins, while
// rivalD4AF, seen only elsewhere, wins at 4,224,534 (34% of the worth);
// matching it still keeps 65% of the surplus. With nothing of this size seen
// from rival2905, it bids like contested rivals at this worth do.
func TestBidCoversARareRivalAtThisWorth(t *testing.T) {
	h := newBidHistory()
	for i := range 40 {
		if i%4 == 3 {
			h.window(route, 50_000)
		} else {
			h.window(route, 50_000, bidContest{from: rival2905, tip: 7})
		}
	}
	for i := range 10 {
		h.window(route, 1_000_000, bidContest{from: rival2905, tip: []uint64{12_224, 100_548, 200_000, 300_000, 521_441}[i%5]})
	}
	for _, worth := range []uint64{10_000_000, 12_000_000, 14_000_000} {
		h.window(elsewhere, worth, bidContest{from: rivalD4AF, tip: worth * 3447 / 10000})
	}
	h.quiet(unrelated, 50_000, 100)
	d := h.decide(route, 12_255_993)
	cap := d.Cap.Uint64()
	if d.Reason != "" || tipOf(d) < 4_224_535 || tipOf(d) > cap || d.Expected.Sign() <= 0 || d.Win < testMinWin {
		t.Fatalf("first recorded send: tip %d (cap %d) win %.3f reason %q, want at least 4,224,535", tipOf(d), cap, d.Win, d.Reason)
	}
	if len(d.Rivals) == 0 || d.Rivals[0].Address != rival2905.Hex() {
		t.Fatalf("first recorded send: rivals %+v", d.Rivals)
	}
	t.Logf("first recorded send: tip %d of cap %d, P(win) %.3f, expected %s, rivals %+v", tipOf(d), cap, d.Win, d.Expected, d.Rivals)
}

// An opportunity worth 1,197,981 per gas, ceiling 701,134. Reading bids from
// windows it never priced, a model predicts the main rival at 1,323,789 and
// sends only 240,021, while rivalA408 wins at 316,065. A rival's bid without
// a known worth says nothing about its share, so the global share decides,
// and a tip that beats a quarter of the worth is affordable.
func TestBidDoesNotChaseUnpricedBids(t *testing.T) {
	for _, seen := range []bool{true, false} {
		h := newBidHistory()
		for i := range 20 {
			if i < 12 {
				h.window(route, 0, bidContest{from: rival2905, tip: []uint64{1_100_000, 1_300_000, 1_500_000}[i%3]})
			} else {
				h.window(route, 0)
			}
		}
		// With 0xa40833c4 seen elsewhere at 26% of the worth, and without it,
		// when only the share prior speaks for this worth.
		for i := range 6 {
			worth := []uint64{900_000, 1_200_000, 1_500_000}[i%3]
			if seen {
				h.window(elsewhere, worth, bidContest{from: rivalA408, tip: worth * 264 / 1000})
			} else {
				h.window(elsewhere, worth)
			}
		}
		h.quiet(unrelated, 50_000, 100)
		d := h.decide(route, 1_197_981)
		t.Logf("second recorded send (winner seen %v): tip %d of cap %d, P(win) %.3f, expected %s", seen, tipOf(d), d.Cap.Uint64(), d.Win, d.Expected)
		if d.Reason != "" || tipOf(d) > d.Cap.Uint64() || d.Expected.Sign() <= 0 || d.RivalBid != 0 {
			t.Fatalf("second recorded send (winner seen %v): %+v", seen, d)
		}
		// Unseen, the winner is one draw from a share prior spread over 5–60%
		// of the worth against a rival present on about 40% of windows; a
		// tenth of the worth has the best expectation. Seen at 26%, it is
		// outbid.
		if seen && tipOf(d) < 316_066 || !seen && tipOf(d) < 119_799 {
			t.Fatalf("second recorded send (winner seen %v): tip %d", seen, tipOf(d))
		}
	}
}

// Nonces 242–245: rivals bid beyond the ceiling on nearly every window, and
// tips of 390–683 wei were sent because a revert costs little. Now the send
// is skipped, with the reason recorded: unlikely, or with the tip floor
// no expected profit at all.
func TestBidSkipsLotteries(t *testing.T) {
	h := newBidHistory()
	for i := range 60 {
		if i%20 == 0 {
			h.window(route, 1_000_000)
		} else {
			h.window(route, 1_000_000, bidContest{from: rivalD4AF, tip: []uint64{700_000, 800_000, 900_000}[i%3]})
		}
	}
	h.quiet(unrelated, 50_000, 100)
	d := h.decide(route, 1_000_000)
	if d.Reason != planning.BidUnlikely && d.Reason != planning.BidNoProfit || d.Win >= testMinWin {
		t.Fatalf("lottery: tip %d win %.3f expected %s reason %q", tipOf(d), d.Win, d.Expected, d.Reason)
	}
	t.Logf("lottery: best tip %d, P(win) %.3f, expected %s, skipped as %s", tipOf(d), d.Win, d.Expected, d.Reason)
}

// predation is a rival that bids several times the worth on every window of
// the route.
func predation(h *bidHistory, n int) {
	for i := range n {
		h.window(route, 1_000_000, bidContest{from: predator, tip: []uint64{2_000_000, 5_000_000, 30_000_000}[i%3]})
	}
}

// A rival bidding above value is not fought: the route is skipped as
// predatory_rival, with a zero-tip probe at most every bidProbeEvery.
func TestBidLeavesAPredatoryRivalAlone(t *testing.T) {
	h := newBidHistory()
	predation(h, 60)
	h.quiet(unrelated, 50_000, 60)
	probe := h.decide(route, 1_000_000)
	if !probe.Probe || tipOf(probe) != 0 || probe.Reason != "" || probe.Expected.Sign() < 0 {
		t.Fatalf("first decision: %+v, want a zero-tip probe", probe)
	}
	if len(probe.Rivals) == 0 || probe.Rivals[0].Address != predator.Hex() || probe.Rivals[0].AboveValue < bidPredatorRate {
		t.Fatalf("predator not reported: %+v", probe.Rivals)
	}
	h.at = h.at.Add(time.Minute)
	if d := h.decide(route, 1_000_000); d.Reason != planning.BidPredatory || d.Probe {
		t.Fatalf("within the probe interval: %+v", d)
	}
	h.at = h.at.Add(bidProbeEvery)
	if d := h.decide(route, 1_000_000); !d.Probe {
		t.Fatalf("after the probe interval: %+v", d)
	}
}

// A predator on one route must not raise our bids elsewhere. Its
// above-value bids enter the global share distribution, where no tip
// within a ceiling beats them and no single rival carries more than
// bidGlobalRivalMass.
func TestBidPredationStaysOnItsRoute(t *testing.T) {
	other := []domain.PoolKey{{Address: domain.Address{0x60}}, {Address: domain.Address{0x61}}}
	build := func(withPredator bool) *execution.BidDecision {
		h := newBidHistory()
		if withPredator {
			predation(h, 60)
		} else {
			h.quiet(route, 1_000_000, 60)
		}
		for i := range 30 {
			if i%3 == 0 {
				h.window(other, 1_000_000)
			} else {
				h.window(other, 1_000_000, bidContest{from: rivalA408, tip: []uint64{100_000, 120_000, 150_000}[i%3]})
			}
		}
		h.quiet(unrelated, 50_000, 60)
		return h.decide(other, 1_000_000)
	}
	clean, dirty := build(false), build(true)
	if dirty.Reason != "" || clean.Reason != "" || tipOf(dirty) != tipOf(clean) {
		t.Fatalf("other route: tip %d (%q) with a predator elsewhere, %d (%q) without", tipOf(dirty), dirty.Reason, tipOf(clean), clean.Reason)
	}

	heavy := &rivalStats{address: predator, ownMass: 100}
	light := &rivalStats{address: rivalA408, ownMass: 2}
	heavy.own = []shareAtom{{0.9, 50}, {3, 50}}
	light.own = []shareAtom{{0.1, 2}}
	var heavyWeight, above float64
	for _, a := range globalShares([]*rivalStats{heavy, light}) {
		if a.share == 0.9 || a.share == 3 {
			heavyWeight += a.weight
		}
		if a.share > 1 {
			above += a.weight
		}
	}
	if above == 0 {
		t.Fatal("above-value shares left out of the global distribution")
	}
	if limit := bidGlobalRivalMass / (bidGlobalRivalMass + 2 + bidPriorStrength); heavyWeight > limit+1e-9 {
		t.Fatalf("one rival carries %.3f of the global shares, limit %.3f", heavyWeight, limit)
	}
}

// Predation is costly and usually brief. Once the predator stops and the
// route's windows are quiet again, its above-value rate and presence fade and
// the route is bid as usual.
func TestBidPredationFades(t *testing.T) {
	h := newBidHistory()
	predation(h, 60)
	h.quiet(unrelated, 50_000, 60)
	if d := h.decide(route, 1_000_000); !d.Probe && d.Reason != planning.BidPredatory {
		t.Fatalf("during predation: %+v", d)
	}
	for range 120 { // two hours, a quiet window on the route each minute
		h.window(route, 1_000_000)
		h.at = h.at.Add(time.Minute)
	}
	d := h.decide(route, 1_000_000)
	if d.Reason != "" || d.Probe || d.Win < testMinWin {
		t.Fatalf("after the predator stopped: tip %d win %.3f reason %q probe %v", tipOf(d), d.Win, d.Reason, d.Probe)
	}
	if len(d.Rivals) > 0 && d.Rivals[0].Address == predator.Hex() && d.Rivals[0].AboveValue >= bidPredatorRate {
		t.Fatalf("predator still marked: %+v", d.Rivals[0])
	}
}

// Baiting: a rival alternating bids far below and far above the worth. The
// tip never exceeds the ceiling, so a win never pays more than the surplus
// (tip × gas limit ≤ surplus), and the route is not locked out for good.
func TestBidCannotBeBaitedAboveItsValue(t *testing.T) {
	h := newBidHistory()
	for i := range 60 {
		tip := uint64(10_000)
		if i%2 == 1 {
			tip = 3_000_000
		}
		h.window(route, 1_000_000, bidContest{from: predator, tip: tip})
	}
	h.quiet(unrelated, 50_000, 60)
	for _, worth := range []uint64{200_000, 1_000_000, 5_000_000} {
		d := h.decide(route, worth)
		surplus := new(big.Int).SetUint64(worth * 300_000)
		paid := new(big.Int).Mul(d.ModelTip, big.NewInt(testGasLimit))
		if d.ModelTip.Cmp(d.Cap) > 0 || paid.Cmp(surplus) > 0 {
			t.Fatalf("worth %d: tip %s above cap %s", worth, d.ModelTip, d.Cap)
		}
	}
	for range 120 {
		h.window(route, 1_000_000)
		h.at = h.at.Add(time.Minute)
	}
	if d := h.decide(route, 1_000_000); d.Reason != "" {
		t.Fatalf("still locked out after the baiting stopped: %+v", d)
	}
}

func TestBidDecisionIsDeterministic(t *testing.T) {
	run := func() []*execution.BidDecision {
		h := newBidHistory()
		for i := range 60 {
			rivals := []domain.Address{rival2905, rivalD4AF, rivalA408, predator}
			var contest []bidContest
			for j, r := range rivals {
				if (i+j)%3 == 0 {
					contest = append(contest, bidContest{from: r, tip: uint64(1000 * (i + 1) * (j + 1))})
				}
			}
			h.window(route, uint64(100_000*(1+i%7)), contest...)
		}
		return []*execution.BidDecision{h.decide(route, 300_000), h.decide(elsewhere, 3_000_000), h.decide(route, 300_000)}
	}
	a, b := run(), run()
	for i := range a {
		if tipOf(a[i]) != tipOf(b[i]) || a[i].Win != b[i].Win || len(a[i].Rivals) != len(b[i].Rivals) {
			t.Fatalf("decision %d differs: %+v vs %+v", i, a[i], b[i])
		}
		for j := range a[i].Rivals {
			if a[i].Rivals[j] != b[i].Rivals[j] {
				t.Fatalf("decision %d rival %d differs", i, j)
			}
		}
	}
	if tipOf(a[0]) != tipOf(a[2]) || a[0].Win != a[2].Win {
		t.Fatal("deciding changed the model")
	}
}

// A rival that wins nearly half the frames we lose, almost all on
// opportunities above $0.001, would get about 1% presence if it were counted
// over all windows: it skips the many small opportunities, which would count
// as its absences. Its presence at a large worth is its rate among windows
// of that size.
func TestBidPresenceIsMeasuredAtTheOpportunitysWorth(t *testing.T) {
	h := newBidHistory()
	strong := domain.Address{0x42, 0x1b, 0xf0, 0x5c}
	for i := range 300 {
		if i%3 == 0 {
			h.window(unrelated, 40_000, bidContest{from: rival2905, tip: 300})
		} else {
			h.window(unrelated, 40_000)
		}
	}
	for i := range 30 {
		if i%5 != 4 {
			h.window(elsewhere, 4_000_000, bidContest{from: strong, tip: 1_000_000 + uint64(i)*40_000})
		} else {
			h.window(elsewhere, 4_000_000)
		}
	}
	d := h.decide(route, 4_000_000)
	var presence float64
	for _, r := range d.Rivals {
		if r.Address == strong.Hex() {
			presence = r.Presence
		}
	}
	if presence < 0.4 {
		t.Fatalf("presence of the large-opportunity rival %.3f, rivals %+v", presence, d.Rivals)
	}
}

// A flat probability curve supplies no evidence that paying a fixed share
// improves success. The model may select zero on a quiet, unsupported route.
func TestBidDecisionDoesNotInventAProfitShareFloor(t *testing.T) {
	h := newBidHistory()
	h.quiet(unrelated, 50_000, 60)
	d := h.decide(route, 1_017_279_228)
	if d.Reason != "" || tipOf(d) != 0 {
		t.Fatalf("tip %d reason %q", tipOf(d), d.Reason)
	}
}

// Robinhood orders first come, first served: a tip buys no position. The
// decision signs a zero tip, and its win probability does not depend on what
// rivals bid, only on how often they show up; a rival bidding above our worth
// is no reason to stay away.
func TestBidUnderArrivalOrderingSignsNoTip(t *testing.T) {
	h := newBidHistory()
	for i := range 60 {
		if i%2 == 0 {
			h.window(route, 1_000_000)
		} else {
			h.window(route, 1_000_000, bidContest{from: predator, tip: 5_000_000})
		}
	}
	h.quiet(unrelated, 50_000, 60)
	r := &reactiveRuntime{bids: h.e}
	r.settings.document.BidMinSamples = 50
	r.settings.document.BidModel = "apply"
	r.settings.minWin = testMinWin
	r.settings.planning.Ordering = domain.OrderingArrival
	surplus := new(big.Int).SetUint64(1_000_000 * 300_000)
	d := r.bidDecisionAt(h.at, route, surplus, testGasLimit, big.NewInt(300), big.NewInt(4_000_000_000), new(big.Int))
	if d.Reason != "" || tipOf(d) != 0 || d.Probe || d.Expected.Sign() <= 0 {
		t.Fatalf("arrival ordering: %+v", d)
	}
	h.e.mu.Lock()
	m := arrivalWinModel(h.e.model(h.at, route, 1_000_000).win)
	h.e.mu.Unlock()
	if m.Raw(0) != m.Raw(maxBid-1) || m.Raw(0) >= 0.7 {
		t.Fatalf("P(win) %.3f at no tip, %.3f at any tip: want equal and below the rival's absence", m.Raw(0), m.Raw(maxBid-1))
	}
}

// One contract can take hundreds of transactions from dozens of senders.
// Counted by sender, each has a few percent presence and the model predicts
// wins far above the observed rate. A rival is the contract its senders call.
func TestBidCountsRotatingSendersAsOneRival(t *testing.T) {
	h := newBidHistory()
	contract := domain.Address{0x69, 0xbb}
	const worth = 1_000_000
	for i := range 120 {
		sender := domain.Address{0xee, byte(i % 40)}
		h.window(route, worth, bidContest{from: sender, to: contract, tip: worth * 6 / 10})
	}
	d := h.decide(route, worth)
	if d.Reason == "" && tipOf(d) <= worth*6/10 {
		t.Fatalf("bid %d under a rival that always bids %d", tipOf(d), worth*6/10)
	}
	if len(d.Rivals) == 0 || d.Rivals[0].Address != contract.Hex() || d.Rivals[0].Presence < 0.8 {
		t.Fatalf("rotating senders not modelled as their contract: %+v", d.Rivals)
	}
}
