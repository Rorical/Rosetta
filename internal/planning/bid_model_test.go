package planning

import (
	"math"
	"math/big"
	"testing"
)

func always(tip uint64) []BidAtom { return []BidAtom{{Tip: tip, Weight: 1}} }

func tipInput(surplus int64, ceiling int64, minWin float64) TipInput {
	return TipInput{Surplus: big.NewInt(surplus), Gas: 400_000, Base: big.NewInt(300), Extra: big.NewInt(20_000_000_000), Ceiling: big.NewInt(ceiling), MinWin: minWin}
}

func TestChooseTipAgainstContenders(t *testing.T) {
	const surplus = 4_000_000_000_000 // 1e7 per gas
	cases := []struct {
		name   string
		model  WinModel
		in     TipInput
		want   uint64
		reason string
	}{
		{"uncontested bids nothing", WinModel{}, tipInput(surplus, 10_000_000, 0.2), 0, ""},
		{"a sure rival is outbid by one wei", WinModel{Contenders: []Contender{{Presence: 1, Bids: always(1_000_000)}}}, tipInput(surplus, 10_000_000, 0.2), 1_000_001, ""},
		{"a rare rival is not worth outbidding", WinModel{Contenders: []Contender{{Presence: 0.05, Bids: always(5_000_000)}}}, tipInput(surplus, 10_000_000, 0.2), 0, ""},
		{"landing first lowers every tip alike", WinModel{First: 0.5, Contenders: []Contender{{Presence: 1, Bids: always(1_000_000)}}}, tipInput(surplus, 10_000_000, 0.2), 1_000_001, ""},
		{"a rival above the ceiling is not reachable", WinModel{Contenders: []Contender{{Presence: 1, Bids: always(10_000_000)}}}, tipInput(surplus, 9_999_999, 0.2), 0, BidNoProfit},
		{"two rivals: the cheaper one is beaten when the dear one is rare",
			WinModel{Contenders: []Contender{{Presence: 0.9, Bids: always(300_000)}, {Presence: 0.1, Bids: always(9_000_000)}}}, tipInput(surplus, 10_000_000, 0.2), 300_001, ""},
	}
	for _, c := range cases {
		got := ChooseTip(c.model, c.in)
		if got.Tip.Uint64() != c.want || got.Reason != c.reason {
			t.Errorf("%s: tip %s reason %q win %.3f, want %d %q", c.name, got.Tip, got.Reason, got.Win, c.want, c.reason)
		}
	}
}

// When rivals bid beyond the surplus on nearly every window, a tiny tip still
// shows a positive expectation because a revert is cheap. Each such send
// reverts and keeps the sender's other routes off those pools. A positive
// expectation that rests on a win probability below the minimum is a skip.
func TestChooseTipSkipsLotteries(t *testing.T) {
	model := WinModel{Contenders: []Contender{{Name: "a", Presence: 0.9, Bids: always(20_000_000)}}}
	lottery := ChooseTip(model, tipInput(4_000_000_000_000, 10_000_000, 0.2))
	if lottery.Reason != BidUnlikely || lottery.Expected.Sign() <= 0 || lottery.Win >= 0.2 || lottery.Limiter != "a" {
		t.Fatalf("lottery: %+v win %.3f", lottery, lottery.Win)
	}
	if open := ChooseTip(model, tipInput(4_000_000_000_000, 10_000_000, 0)); open.Reason != "" || open.Tip.Sign() != 0 {
		t.Fatalf("a zero minimum disables the gate: %+v", open)
	}
	// Above the minimum, the gate picks the best tip that clears it rather
	// than a cheaper lottery.
	split := WinModel{Contenders: []Contender{{Presence: 0.9, Bids: []BidAtom{{1_000_000, 0.85}, {20_000_000, 0.15}}}}}
	chosen := ChooseTip(split, tipInput(4_000_000_000_000, 10_000_000, 0.2))
	if chosen.Reason != "" || chosen.Tip.Uint64() != 1_000_001 || chosen.Win < 0.2 {
		t.Fatalf("gated choice: %+v win %.3f", chosen, chosen.Win)
	}
}

// A rival that bids above the opportunity's value is named as the reason, and
// a zero-tip probe is offered when it costs nothing in expectation.
func TestChooseTipNamesPredatoryRivals(t *testing.T) {
	model := WinModel{Contenders: []Contender{{Name: "p", Presence: 0.8, Bids: always(30_000_000), Predatory: true}, {Name: "q", Presence: 0.1, Bids: always(5)}}}
	got := ChooseTip(model, tipInput(4_000_000_000_000, 10_000_000, 0.2))
	if got.Reason != BidPredatory || got.Limiter != "p" || got.Probe == nil || got.Probe.Tip.Sign() != 0 || got.Probe.Expected.Sign() < 0 {
		t.Fatalf("predator: %+v", got)
	}
	// When the rival that holds the win probability down is not the predator,
	// the reason is the ordinary one.
	model.Contenders[0].Predatory, model.Contenders[1].Predatory = false, true
	if got := ChooseTip(model, tipInput(4_000_000_000_000, 10_000_000, 0.2)); got.Reason != BidUnlikely || got.Probe != nil {
		t.Fatalf("non-predatory limiter: %+v", got)
	}
}

// The ceiling (policy maximum, fee cap, budget, surplus per unit of gas
// limit) is never crossed, however a rival bids.
func TestChooseTipNeverExceedsTheCeiling(t *testing.T) {
	for _, bid := range []uint64{1, 999_999, 1_000_000, 1_000_001, 50_000_000, math.MaxUint64 - 1} {
		model := WinModel{Contenders: []Contender{{Presence: 1, Bids: []BidAtom{{bid, 0.5}, {bid / 2, 0.5}}}}}
		for _, minWin := range []float64{0, 0.2} {
			got := ChooseTip(model, tipInput(4_000_000_000_000, 1_000_000, minWin))
			if got.Tip.Uint64() > 1_000_000 {
				t.Fatalf("bid %d: tip %s above the ceiling", bid, got.Tip)
			}
		}
	}
}

func TestCalibrateShiftsTowardOutcomes(t *testing.T) {
	if Calibrate(0.37, 0) != 0.37 {
		t.Fatal("a zero shift changed the probability")
	}
	down, up := Calibrate(0.9, -1), Calibrate(0.9, 1)
	if down >= 0.9 || up <= 0.9 || down <= 0 || up >= 1 {
		t.Fatalf("shifted 0.9 to %.3f and %.3f", down, up)
	}
	if Calibrate(0.2, -1) >= Calibrate(0.3, -1) {
		t.Fatal("calibration does not keep the order of probabilities")
	}
}

// With the executor price precheck a lost auction reverts before its
// swaps, so the gas at risk is a fixed small amount instead of half the route.
// A bid that only pays when losing is cheap must then be taken.
func TestChooseTipPricesLossesAtTheRevertGas(t *testing.T) {
	const surplus = 4_000_000_000_000
	model := WinModel{First: 0.6, Contenders: []Contender{{Presence: 0.9, Bids: always(6_000_000)}}}
	in := tipInput(surplus, 10_000_000, 0)
	if got := ChooseTip(model, in); got.Tip.Uint64() != 0 {
		t.Fatalf("without the precheck a costly loss should keep the tip at 0, got %s", got.Tip)
	}
	in.RevertGas = 60_000
	if got := ChooseTip(model, in); got.Tip.Uint64() != 6_000_001 {
		t.Fatalf("with cheap losses the rival should be outbid, got %s", got.Tip)
	}
}

// The lowest tip the model expects to clear the minimum win probability can
// be orders of magnitude under what a rival pays (an opportunity worth 1.0e9
// wei per gas sent at 57,787 loses to a rival at 40.2M, 4% of the worth). A
// floor keeps every candidate tip at or
// above it, still below the ceiling.
func TestChooseTipRespectsTheFloor(t *testing.T) {
	uncontested := tipInput(4_000_000_000_000, 10_000_000, 0.2)
	uncontested.Floor = 1_500_000
	if got := ChooseTip(WinModel{}, uncontested); got.Tip.Uint64() != 1_500_000 || got.Reason != "" {
		t.Fatalf("uncontested: tip %s reason %q", got.Tip, got.Reason)
	}
	rival := WinModel{Contenders: []Contender{{Presence: 1, Bids: always(3_000_000)}}}
	if got := ChooseTip(rival, uncontested); got.Tip.Uint64() != 3_000_001 {
		t.Fatalf("a rival above the floor is still outbid: tip %s", got.Tip)
	}
	above := tipInput(4_000_000_000_000, 1_000_000, 0.2)
	above.Floor = 1_500_000
	if got := ChooseTip(WinModel{}, above); got.Tip.Uint64() != 1_000_000 {
		t.Fatalf("a floor above the ceiling: tip %s", got.Tip)
	}
}
