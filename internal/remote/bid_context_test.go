package remote

import (
	"github.com/Rorical/Rosetta/internal/planning"
	"math/big"
	"testing"
	"time"
)

func TestCalibrationLearnsAgeAndLaneWithoutExtrapolation(t *testing.T) {
	var c calibration
	now := time.Unix(1800000000, 0)
	for i := range 240 {
		age := int64(20 * time.Millisecond)
		if i%2 == 0 {
			age = int64(180 * time.Millisecond)
		}
		lane := "full"
		if i%4 < 2 {
			lane = "fast"
		}
		won := i%2 != 0 && (lane == "full" || i%8 < 4)
		c.add(calibrationPoint{raw: .6, worth: 1000, ageNS: age, lane: lane, won: won, at: now.Add(time.Duration(i) * time.Second)})
	}
	young := c.shiftAt(1000, int64(20*time.Millisecond), "full")
	old := c.shiftAt(1000, int64(180*time.Millisecond), "full")
	if young <= old || c.shiftAt(1000, int64(time.Second), "full") != old {
		t.Fatal(young, old, c.ageSlope)
	}
	e := newBidEstimator(time.Second, 2*time.Second)
	e.calibration = c
	e.observeRevertGas(900000, 1000000, false)
	raw, err := e.encode(now.Add(5 * time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	restored := newBidEstimator(time.Second, 2*time.Second)
	if err = restored.decode(raw, now.Add(6*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if restored.calibration.shiftAt(1000, int64(20*time.Millisecond), "full") != young || len(restored.revertRatios) != 1 {
		t.Fatal("context or cost lost on restart")
	}
}

func TestJointDecisionUsesTriggerAgeAndRevertTail(t *testing.T) {
	h := newBidHistory()
	h.quiet(unrelated, 100000, 60)
	runtime := &reactiveRuntime{bids: h.e}
	runtime.settings.document.BidModel = "apply"
	runtime.settings.planning.PricePrecheck = true
	for range 20 {
		h.e.observeRevertGas(900000, 1000000, false)
	}
	request := planning.BidRequest{Route: route, Surplus: big.NewInt(100000000000), Gas: 1000000, Base: big.NewInt(300), Extra: big.NewInt(1), Ceiling: big.NewInt(100000), ReceivedAt: h.at.Add(-40 * time.Millisecond), Lane: "fast", MinimumTip: big.NewInt(500)}
	d := runtime.priceBid(h.at, request)
	if d.AgeNS != int64(40*time.Millisecond) || d.RevertGas < 700000 || d.Lane != "fast" || d.ModelTip.Int64() < 500 {
		t.Fatal(d)
	}
	request.MinimumTip = big.NewInt(200000)
	if got := runtime.priceBid(h.at, request); got.Reason != planning.BidNoProfit {
		t.Fatal(got)
	}
}

// Conditional losses learn apart from plain ones: a dropped send counts as a
// free loss, and the route's own prior replaces the precheck's.
func TestConditionalLossesLearnApart(t *testing.T) {
	e := newBidEstimator(time.Second, 2*time.Second)
	e.observeRevertGas(600000, 1000000, false)
	if got := e.expectedRevertGasLocked(1000000, 400000, true, 50000, true); got != 50000 {
		t.Fatal("a plain revert moved the conditional estimate", got)
	}
	for range gasMinRatios {
		e.observeFreeLoss()
	}
	if got := e.expectedRevertGasLocked(1000000, 400000, true, 50000, true); got != 25000 {
		t.Fatal("free losses did not halve the conditional estimate", got)
	}
	if got := e.expectedRevertGasLocked(1000000, 400000, true, 0, false); got != (5*precheckRevertGas+600000)/6 {
		t.Fatal("plain estimate", got)
	}
	raw, err := e.encode(time.Now())
	if err != nil {
		t.Fatal(err)
	}
	restored := newBidEstimator(time.Second, 2*time.Second)
	if err := restored.decode(raw, time.Now()); err != nil || len(restored.conditionalRatios) != gasMinRatios || len(restored.revertRatios) != 1 {
		t.Fatal("conditional losses not restored", err, len(restored.conditionalRatios))
	}
}
