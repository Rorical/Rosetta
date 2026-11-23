package remote

import (
	"math"
	"math/big"
	"testing"
	"time"

	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
)

// sizedSend is one settled send: the worth per gas it was priced at, the
// model's raw win probability for its tip and whether it won.
type sizedSend struct {
	worth uint64
	raw   float64
	won   bool
}

// Worth per gas against the quoted gross: an opportunity worth 12,255,993 wei
// per gas is about $0.014, so $0.0003 is about 263,000 and $0.003 about
// 2,630,000.
var sendSizes = []struct {
	name       string
	low, high  uint64
	sent, wins int
}{
	{"below $0.0003", 0, 263_000, 44, 35},
	{"$0.0003–0.001", 263_000, 876_000, 18, 7},
	{"$0.001–0.003", 876_000, 2_630_000, 4, 1},
	{"$0.003 and above", 2_630_000, math.MaxUint64, 4, 0},
}

// recordedSizedSends reproduces a recorded half hour of 70 sends by size,
// interleaved. The model predicted 0.35–0.6 throughout, and 0.38–0.51
// for the four largest, which lost to rivals bidding 30–96% of their worth.
func recordedSizedSends() []sizedSend {
	worths := [][]uint64{
		{60_000, 90_000, 120_000, 160_000, 200_000, 240_000},
		{300_000, 450_000, 600_000, 800_000},
		{1_000_000, 1_500_000, 2_000_000, 2_500_000},
		{2_800_000, 3_500_000, 4_500_000, 5_500_000},
	}
	raws := [][]float64{{0.35, 0.45, 0.5, 0.6}, {0.45, 0.5, 0.55}, {0.5, 0.45, 0.55, 0.5}, {0.38, 0.45, 0.51, 0.42}}
	var all []sizedSend
	for b, size := range sendSizes {
		for i := range size.sent {
			// i·7 mod n spreads the wins over the bucket; 7 is coprime to every n.
			won := (i*7)%size.sent < size.wins
			all = append(all, sizedSend{worths[b][i%len(worths[b])], raws[b][i%len(raws[b])], won})
		}
	}
	// 19 is coprime to 70: every send once, the sizes interleaved.
	out := make([]sizedSend, len(all))
	for i := range out {
		out[i] = all[(i*19)%len(all)]
	}
	return out
}

const sendEvery = 28 * time.Second // 70 sends in 33 minutes

// One intercept cannot fit both ends: small opportunities won more often than
// predicted and large ones less. Calibrated by size, each bucket's mean
// predicted win probability moves toward its observed rate.
func TestCalibrationFollowsOutcomesBySize(t *testing.T) {
	var c calibration
	start := time.Unix(1_800_000_000, 0)
	sends := recordedSizedSends()
	for i, s := range sends {
		c.add(calibrationPoint{raw: s.raw, worth: s.worth, won: s.won, at: start.Add(time.Duration(i) * sendEvery)})
	}
	if c.slope >= 0 {
		t.Fatalf("slope %.3f, want negative: larger opportunities won less than predicted", c.slope)
	}
	for _, size := range sendSizes {
		var raw, calibrated float64
		var n, wins int
		for _, s := range sends {
			if s.worth < size.low || s.worth >= size.high {
				continue
			}
			raw += s.raw
			calibrated += planning.Calibrate(s.raw, c.shift(s.worth))
			n++
			if s.won {
				wins++
			}
		}
		if n != size.sent || wins != size.wins {
			t.Fatalf("%s: %d of %d won, want %d of %d", size.name, wins, n, size.wins, size.sent)
		}
		raw, calibrated = raw/float64(n), calibrated/float64(n)
		observed := float64(wins) / float64(n)
		if math.Abs(calibrated-observed) >= math.Abs(raw-observed) {
			t.Fatalf("%s: predicted %.3f raw, %.3f calibrated, observed %.3f", size.name, raw, calibrated, observed)
		}
		t.Logf("%s: predicted %.3f raw, %.3f calibrated, observed %.3f (%d sends)", size.name, raw, calibrated, observed, n)
	}
	t.Logf("intercept %.3f, slope %.3f per e-fold around %.0f wei/gas", c.intercept, c.slope, math.Exp(c.center))
}

// Two outcomes at opposite ends of the worth range, which a steeper slope
// always fits better, move the shift by less than one logit anywhere; worth
// beyond the outcomes' range is not extrapolated; outcomes without a worth
// (format 2) leave the slope at zero.
func TestCalibrationBySizeIsStableOnFewOutcomes(t *testing.T) {
	start := time.Unix(1_800_000_000, 0)
	var c calibration
	c.add(calibrationPoint{raw: 0.5, worth: 100_000, won: true, at: start})
	c.add(calibrationPoint{raw: 0.5, worth: 4_000_000, won: false, at: start.Add(sendEvery)})
	if c.slope >= 0 || c.slope < -0.6 {
		t.Fatalf("two outcomes moved the slope to %.3f", c.slope)
	}
	for _, worth := range []uint64{1, 100_000, 4_000_000, math.MaxUint64} {
		if shift := c.shift(worth); math.Abs(shift) >= 1 {
			t.Fatalf("two outcomes shift worth %d by %.3f", worth, shift)
		}
	}
	if c.shift(40_000_000) != c.shift(4_000_000) || c.shift(1_000) != c.shift(100_000) {
		t.Fatal("the slope was extrapolated beyond the outcomes' worth")
	}

	var legacy calibration
	for i := range 40 {
		legacy.add(calibrationPoint{raw: 0.5, won: i%4 != 0, at: start.Add(time.Duration(i) * sendEvery)})
	}
	if legacy.slope != 0 || legacy.intercept <= 0 || legacy.shift(5_000_000) != legacy.intercept {
		t.Fatalf("outcomes without worth: intercept %.3f slope %.3f", legacy.intercept, legacy.slope)
	}
}

// The four large sends of the recorded half hour were predicted to win
// 0.38–0.51, bid 600–740k wei per gas and lost to rivals bidding 30–96% of
// their worth, each revert costing $0.0005–0.0007. Their route's recent
// windows show 0x2905b57c at 12–15% of the worth, beaten by such a tip, and
// two rivals at 30–96% on most. With the 70 outcomes of that half hour, an
// opportunity there worth $0.0045 (3,940,000 wei per gas) wins less often
// than the minimum at every affordable tip and is skipped. One intercept
// fitted to the same outcomes, the earlier size calibration, bids about 600k.
func TestBidSkipsALargeOpportunityThatSizeCalibrationExpectsToLose(t *testing.T) {
	const worth = 3_940_000
	build := func(sized bool) (*bidHistory, *execution.BidDecision) {
		h := newBidHistory()
		for i := range 120 {
			small := []uint64{60_000, 120_000, 200_000, 450_000}[i%4]
			if i%2 == 0 {
				h.window(unrelated, small, bidContest{from: rival2905, tip: small * []uint64{2, 5, 10}[i%3] / 100})
			} else {
				h.window(unrelated, small)
			}
		}
		for i := range 12 {
			large := []uint64{2_800_000, 3_500_000, 4_500_000}[i%3]
			contest := []bidContest{{from: rival2905, tip: large * []uint64{12, 13, 14, 15}[i%4] / 100}}
			if i != 5 {
				contest = append(contest, bidContest{from: rivalD4AF, tip: large * []uint64{30, 55, 65, 96}[i%4] / 100})
			}
			if i%4 != 1 {
				contest = append(contest, bidContest{from: rivalA408, tip: large * []uint64{45, 60, 75}[i%3] / 100})
			}
			h.window(route, large, contest...)
		}
		for _, s := range recordedSizedSends() {
			decision := &execution.BidDecision{ModelTip: big.NewInt(1000), Win: s.raw, WinRaw: s.raw, Applied: true}
			if sized {
				decision.Worth = s.worth
			}
			h.e.settle(bidOutcomeOf(settlement(0, s.won, decision), h.at))
			h.at = h.at.Add(sendEvery)
		}
		return h, h.decide(route, worth)
	}

	_, pooled := build(false)
	t.Logf("one intercept: tip %d of cap %d, P(win) %.3f, shift %.3f", tipOf(pooled), pooled.Cap.Uint64(), pooled.Win, pooled.Shift)
	// Presence measured at this worth puts its win near 0.3; the
	// point here is the tip, which the size calibration below withholds.
	if pooled.Reason != "" || tipOf(pooled) < 500_000 || pooled.Win < testMinWin {
		t.Fatalf("one intercept: tip %d P(win) %.3f reason %q, want the recorded send near 600k", tipOf(pooled), pooled.Win, pooled.Reason)
	}

	h, d := build(true)
	if d.Reason != planning.BidUnlikely && d.Reason != planning.BidNoProfit {
		t.Fatalf("by size: tip %d P(win) %.3f reason %q, want the opportunity skipped", tipOf(d), d.Win, d.Reason)
	}
	if d.ShiftSlope >= 0 || d.Shift >= d.ShiftIntercept || d.ShiftCenter == 0 {
		t.Fatalf("by size: shift %.3f, intercept %.3f, slope %.3f around %d", d.Shift, d.ShiftIntercept, d.ShiftSlope, d.ShiftCenter)
	}
	// P(win) rises with the tip, so the cap bounds every affordable tip.
	h.e.mu.Lock()
	m := h.e.model(h.at, route, worth)
	h.e.mu.Unlock()
	top := planning.Calibrate(m.win.Raw(d.Cap.Uint64()), m.win.Shift)
	if top >= testMinWin {
		t.Fatalf("by size: P(win) %.3f at the cap %d", top, d.Cap.Uint64())
	}
	t.Logf("by size: skipped as %s; P(win) %.3f at the cap %d, shift %.3f (intercept %.3f, slope %.3f per e-fold around %d)", d.Reason, top, d.Cap.Uint64(), d.Shift, d.ShiftIntercept, d.ShiftSlope, d.ShiftCenter)
}
