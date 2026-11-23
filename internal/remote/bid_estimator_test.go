package remote

import (
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

func TestBidEstimatorClassifiesRivalsByWhenTheyLand(t *testing.T) {
	e := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	route := []domain.PoolKey{{Address: domain.Address{1}}, {Address: domain.Address{2}}}
	other := []domain.PoolKey{{Address: domain.Address{9}}}
	a, b := domain.Address{0xa}, domain.Address{0xb}
	start := time.Unix(1000, 0)
	sight := func(pools []domain.PoolKey, from domain.Address, tip uint64, at time.Duration) {
		e.observe(rivalSighting{pools: pools, from: from, tip: tip}, start.Add(at))
	}

	// Landed before us: no tip wins it.
	e.register(route, 0, start)
	e.register(route, 0, start.Add(50*time.Millisecond)) // the same route from the fast lane
	sight(route[:1], a, 5_000_000, 100*time.Millisecond)
	// Same flashblock as us: every rival's highest bid is a contest.
	e.register(route, 0, start.Add(time.Second))
	sight(route[1:], a, 300, 1500*time.Millisecond)
	sight(route[1:], a, 9_000, 1600*time.Millisecond)
	sight(route[:1], b, 4_000, 1650*time.Millisecond)
	sight(other, b, 1_000_000, 1600*time.Millisecond) // unrelated pools
	// Nobody came.
	e.register(route, 0, start.Add(3*time.Second))

	e.mu.Lock()
	e.expire(start.Add(10 * time.Second))
	windows := slices.Clone(e.windows)
	e.mu.Unlock()
	if len(windows) != 3 {
		t.Fatalf("%d windows, want 3", len(windows))
	}
	if !windows[0].first || windows[0].firstBy != a || len(windows[0].contest) != 0 {
		t.Fatalf("first lander: %+v", windows[0])
	}
	if want := []bidContest{{from: a, tip: 9_000}, {from: b, tip: 4_000}}; windows[1].first || !slices.Equal(windows[1].contest, want) {
		t.Fatalf("contest: %+v, want %v", windows[1], want)
	}
	if windows[2].first || len(windows[2].contest) != 0 {
		t.Fatalf("quiet: %+v", windows[2])
	}
}

// The tip is paid on gas used, not on the gas limit, so the expected gas
// prices it.
func TestBidDecisionPricesTipOnGasUsed(t *testing.T) {
	h := newBidHistory()
	h.quiet(unrelated, 50_000, 60)
	for range 8 {
		h.e.observeGas(527_812, 1_250_000)
	}
	r := &reactiveRuntime{bids: h.e}
	r.settings.document.BidMinSamples = 50
	r.settings.document.BidModel = "apply"
	d := r.bidDecisionAt(h.at, route, big.NewInt(103_000_000_000), 1_250_000, big.NewInt(300), big.NewInt(4_000_000_000), big.NewInt(10_000_000))
	if d == nil || d.Gas >= 1_250_000 || d.Gas < 527_812 {
		t.Fatalf("tip priced on gas %+v, want expected use", d)
	}
	r.settings.document.BidMinSamples = 61
	if d := r.bidDecisionAt(h.at, route, big.NewInt(103_000_000_000), 1_250_000, big.NewInt(300), big.NewInt(4_000_000_000), big.NewInt(10_000_000)); d != nil {
		t.Fatal("decided before bid_min_samples windows closed")
	}
}

// settlement is one of our sends on route, priced against window, landing in
// block 100, frame 3.
func settlement(window int64, won bool, decision *execution.BidDecision) *execution.Settlement {
	steps := make([]execution.ExecutorStep, len(route))
	for i, key := range route {
		steps[i].Pool = key
	}
	bid := *decision
	bid.Window = window
	intent := &execution.ReactiveIntent{Route: execution.ExecutorPlan{Steps: steps}, Bid: &bid}
	intent.Call.TipCap = new(big.Int).Set(decision.ModelTip)
	return &execution.Settlement{Intent: intent, Success: won, Position: &execution.ExecutionPosition{BlockNumber: 100, FrameIndex: 3, TransactionIndex: 9}}
}

// A rival in our own flashblock that the timing rule took for a first lander
// competed with our tip: our outcome turns it into a contest, and a rival
// behind us in that flashblock counts too.
func TestBidOutcomeAttributesOurFlashblock(t *testing.T) {
	h := newBidHistory()
	h.quiet(unrelated, 50_000, 60)
	start := h.at
	h.e.register(route, 0, start)
	h.e.mu.Lock()
	window := h.e.price(route, 1_000_000, start.Add(5*time.Millisecond))
	h.e.mu.Unlock()
	h.e.observe(rivalSighting{pools: route, from: rivalD4AF, tip: 400_000, block: 100, frame: 3}, start.Add(200*time.Millisecond))
	h.e.observe(rivalSighting{pools: route[:1], from: rival2905, tip: 7, block: 100, frame: 3}, start.Add(210*time.Millisecond))
	h.e.observe(rivalSighting{pools: route, from: rivalA408, tip: 900_000, block: 100, frame: 2}, start.Add(220*time.Millisecond))
	decision := &execution.BidDecision{ModelTip: big.NewInt(300_000), Win: 0.6, WinRaw: 0.6, Applied: true}
	h.e.settle(bidOutcomeOf(settlement(window, false, decision), start.Add(time.Second)))
	h.e.mu.Lock()
	h.e.expire(start.Add(3 * time.Second))
	w, _ := h.e.find(window, route)
	h.e.mu.Unlock()
	if w == nil || w.first || !slices.Contains(w.contest, bidContest{from: rivalD4AF, tip: 400_000}) || !slices.Contains(w.contest, bidContest{from: rival2905, tip: 7}) || slices.ContainsFunc(w.contest, func(c bidContest) bool { return c.from == rivalA408 }) {
		t.Fatalf("window after our outcome: %+v", w)
	}
	if len(h.e.calibration.points) != 1 || h.e.calibration.points[0].won {
		t.Fatalf("calibration points: %+v", h.e.calibration.points)
	}
}

// Our outcomes recalibrate the model: a route the model expects to win nine
// times in ten, which we win half the time, is soon predicted lower.
func TestBidCalibrationFollowsOutcomes(t *testing.T) {
	h := newBidHistory()
	h.quiet(unrelated, 50_000, 60)
	before := h.decide(route, 1_000_000)
	for i := range 80 {
		decision := &execution.BidDecision{ModelTip: big.NewInt(1000), Win: 0.9, WinRaw: 0.9, Applied: true}
		h.e.settle(bidOutcomeOf(settlement(0, i%2 == 0, decision), h.at))
		h.at = h.at.Add(10 * time.Second)
	}
	after := h.decide(route, 1_000_000)
	if after.Shift >= 0 || after.Win >= before.Win || after.Win < 0.4 {
		t.Fatalf("P(win) %.3f before, %.3f after (shift %.3f); outcomes won half", before.Win, after.Win, after.Shift)
	}
	t.Logf("calibration: P(win) %.3f → %.3f, shift %.3f", before.Win, after.Win, after.Shift)

	// A tip the model did not price (shadow mode, policy floors) does not calibrate.
	foreign := &execution.BidDecision{ModelTip: big.NewInt(1000), Win: 0.9, WinRaw: 0.9}
	s := settlement(0, true, foreign)
	s.Intent.Call.TipCap = big.NewInt(2000)
	if bidOutcomeOf(s, h.at).calibrate {
		t.Fatal("an outcome at a tip the model did not choose calibrated it")
	}
}

func TestCalibrationShiftStaysNearZeroOnFewOutcomes(t *testing.T) {
	var c calibration
	at := time.Unix(1000, 0)
	c.add(calibrationPoint{raw: 0.9, at: at})
	if c.intercept >= 0 || c.intercept < -1 {
		t.Fatalf("one loss moved the intercept to %.3f", c.intercept)
	}
	for i := range 200 {
		c.add(calibrationPoint{raw: 0.5, won: i%5 != 0, at: at.Add(time.Duration(i) * time.Second)})
	}
	if c.intercept < 0.8 || c.intercept > 1.8 { // logit(0.8) ≈ 1.39
		t.Fatalf("200 outcomes at 80%% wins against 0.5: intercept %.3f", c.intercept)
	}
}

// A standing opportunity is readmitted with every frame that touches its
// pools. Until a rival acts on it, that is one window, not one quiet window per
// frame; after a rival, the next admission is a new opportunity.
func TestBidWindowsCountOpportunitiesNotAdmissions(t *testing.T) {
	e := newBidEstimator(100*time.Millisecond, 550*time.Millisecond)
	route := []domain.PoolKey{{Address: domain.Address{1}}, {Address: domain.Address{2}}}
	start := time.Unix(1000, 0)
	for i := range 10 {
		e.register(route, 50_000, start.Add(time.Duration(i)*400*time.Millisecond))
	}
	e.observe(rivalSighting{pools: route, from: domain.Address{0xa}, tip: 9_000}, start.Add(3800*time.Millisecond))
	e.register(route, 60_000, start.Add(4*time.Second))
	e.register(route, 0, start.Add(20*time.Second)) // long after: another opportunity
	e.mu.Lock()
	e.expire(start.Add(time.Minute))
	windows := slices.Clone(e.windows)
	e.mu.Unlock()
	if len(windows) != 3 || len(windows[0].contest) != 1 || windows[0].worth != 50_000 || !windows[1].quiet() || !windows[2].quiet() {
		t.Fatalf("windows %+v, want the readmitted opportunity once, then two others", windows)
	}
}

// Closed windows are kept by age: three presence half-lives, more than a
// fixed count of 2048, which can cover under ten minutes of admissions.
func TestBidWindowsAreKeptForTheRetention(t *testing.T) {
	e := newBidEstimator(100*time.Millisecond, 550*time.Millisecond)
	start := time.Unix(1000, 0)
	for i := range 6000 {
		at := start.Add(time.Duration(i) * 2 * time.Second)
		e.register([]domain.PoolKey{{Address: domain.Address{byte(i)}, PoolID: domain.Hash{byte(i >> 8)}}}, 1, at)
	}
	e.mu.Lock()
	e.expire(start.Add(6000 * 2 * time.Second))
	windows := slices.Clone(e.windows)
	e.mu.Unlock()
	if len(windows) <= 2048 || windows[len(windows)-1].last.Sub(windows[0].last) > bidRetention {
		t.Fatalf("%d windows spanning %s", len(windows), windows[len(windows)-1].last.Sub(windows[0].last))
	}
}
