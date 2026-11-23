package remote

import (
	"path/filepath"
	"testing"
	"time"
)

// sent books one of our sends on route at tip, with the highest rival tip in
// our flashblock (0: no rival there).
func (h *bidHistory) sent(worth, tip, rival uint64, won bool) {
	block := uint64(h.at.Unix())
	if rival > 0 {
		h.e.observe(rivalSighting{pools: route, from: rivalD4AF, tip: rival, block: block, frame: 1}, h.at.Add(100*time.Millisecond))
	}
	h.e.settle(bidOutcome{pools: route, worth: worth, tip: tip, won: won, positioned: true, block: block, frame: 1, at: h.at.Add(200 * time.Millisecond)})
	h.at = h.at.Add(bidContinue + time.Second)
	h.e.mu.Lock()
	h.e.expire(h.at)
	h.e.mu.Unlock()
}

// The window model can predict 0.45–0.95 wins where about one send in ten
// wins, most losing to a rival in the same flashblock that tips 3e7–5.6e7 wei
// per gas against 2e5–1e7. Our own sends say what a tip has to beat.
func TestSentOutcomesDecideTheBid(t *testing.T) {
	h := newBidHistory()
	h.quiet(route, 100_000_000, 60) // the window model sees no competition
	if d := h.decide(route, 100_000_000); d.Reason != "" || d.Win < 0.8 {
		t.Fatalf("quiet history: %+v", d)
	}
	for range 2 {
		h.sent(10_000_000, 300_000, 0, true)
	}
	for range 30 {
		h.sent(100_000_000, 5_000_000, 30_000_000, false)
		h.sent(10_000_000, 300_000, 30_000_000, false)
	}
	// Worth more than the rivals bid: outbid them.
	d := h.decide(route, 100_000_000)
	if d.Reason != "" || tipOf(d) <= 30_000_000 || tipOf(d) > 40_000_000 || d.Win < 0.6 {
		t.Fatalf("an opportunity worth more than rivals bid: tip %d win %.2f reason %q", tipOf(d), d.Win, d.Reason)
	}
	// Worth less: no affordable tip wins often enough.
	if d := h.decide(route, 10_000_000); d.Reason == "" || d.Win > 0.2 {
		t.Fatalf("an opportunity rivals outbid was sent: tip %d win %.2f", tipOf(d), d.Win)
	}
	// A size never sent keeps the window model.
	if d := h.decide(route, 100_000_000_000); d.Reason != "" || d.Win < 0.8 {
		t.Fatalf("outcomes at another size decided: %+v", d)
	}
}

func TestSentOutcomesClassifyWhatBeatsThem(t *testing.T) {
	h := newBidHistory()
	h.sent(1_000_000, 500, 0, true)                                            // uncontested
	h.sent(1_000_000, 500, 200, true)                                          // won over a lower bid
	h.sent(1_000_000, 500, 900, false)                                         // outbid
	h.sent(1_000_000, 500, 200, false)                                         // lost to a lower bid: no tip explains it
	h.sent(1_000_000, 500, 0, false)                                           // lost with no rival seen
	h.e.settle(bidOutcome{pools: route, worth: 1_000_000, tip: 500, at: h.at}) // lost, never positioned
	h.e.mu.Lock()
	h.e.expire(h.at.Add(time.Minute))
	var beats []uint64
	for _, o := range h.e.sent {
		beats = append(beats, o.beat)
	}
	h.e.mu.Unlock()
	want := []uint64{0, 200, 900, maxBid, maxBid, maxBid}
	if len(beats) != len(want) {
		t.Fatal(beats)
	}
	for i := range want {
		if beats[i] != want[i] {
			t.Fatalf("outcome %d: beat %d, want %d", i, beats[i], want[i])
		}
	}
	// They survive a restart.
	path := filepath.Join(t.TempDir(), "bids.json")
	if err := h.e.save(path, h.at); err != nil {
		t.Fatal(err)
	}
	restored := newBidEstimator(250*time.Millisecond, 550*time.Millisecond)
	restored.load(path, h.at)
	if len(restored.sent) != len(want) || restored.sent[2].beat != 900 || restored.sent[2].worth != 1_000_000 {
		t.Fatalf("restored: %+v", restored.sent)
	}
}

// A rival whose route holds only one of our pools opens no contest in a
// window, yet it is what our send has to beat (such a rival at 1.4e8 wei per
// gas lands ahead of a send at 3.9e7).
func TestSinglePoolRivalSetsWhatASendHadToBeat(t *testing.T) {
	h := newBidHistory()
	h.e.register(route, 0, h.at)
	h.e.mu.Lock()
	window := h.e.price(route, 90_000_000, h.at.Add(5*time.Millisecond))
	h.e.mu.Unlock()
	h.e.observe(rivalSighting{pools: route[:1], from: rivalD4AF, tip: 140_000_000, block: 9, frame: 1, single: true}, h.at.Add(300*time.Millisecond))
	h.e.settle(bidOutcome{pools: route, window: window, worth: 90_000_000, tip: 39_000_000, positioned: true, block: 9, frame: 1, at: h.at.Add(400 * time.Millisecond)})
	h.e.mu.Lock()
	h.e.expire(h.at.Add(time.Minute))
	w, _ := h.e.find(window, route)
	sent := h.e.sent
	h.e.mu.Unlock()
	if w == nil || len(w.contest) != 0 || w.first {
		t.Fatalf("a one-pool transaction contested the window: %+v", w)
	}
	if len(sent) != 1 || sent[0].beat != 140_000_000 {
		t.Fatalf("sent outcomes: %+v", sent)
	}
}

// Conditional sends can all be dropped for free because a rival moved a pool
// first, at tips an order of magnitude under the rivals'. A dropped send has
// no receipt and no position, so without this the model would go on
// predicting wins of 0.2–0.6 at those tips. A dropped send is a loss to the
// highest rival tip seen on its pools in the block it was quoted in or the
// next two.
func TestDroppedConditionalSendsTeachTheBid(t *testing.T) {
	h := newBidHistory()
	h.quiet(route, 100_000_000, 60) // the window model sees no competition
	for range 20 {
		block := uint64(h.at.Unix())
		// The rival lands in the next block, in a frame we never learn.
		h.e.observe(rivalSighting{pools: route, from: rivalD4AF, tip: 30_000_000, block: block + 1, frame: 2}, h.at.Add(100*time.Millisecond))
		h.e.settle(bidOutcome{pools: route, worth: 100_000_000, tip: 900_000, dropped: true, block: block, at: h.at.Add(200 * time.Millisecond)})
		h.at = h.at.Add(bidContinue + time.Second)
		h.e.mu.Lock()
		h.e.expire(h.at)
		h.e.mu.Unlock()
	}
	d := h.decide(route, 100_000_000)
	if d.Reason != "" || tipOf(d) <= 30_000_000 {
		t.Fatalf("after 20 sends dropped to a 3e7 tip the bid is %d (win %.2f, %q)", tipOf(d), d.Win, d.Reason)
	}
	// A drop with no rival seen on the pools says nothing about tips.
	fresh := newBidHistory()
	fresh.e.settle(bidOutcome{pools: route, worth: 100_000_000, tip: 900_000, dropped: true, block: 7, at: fresh.at})
	fresh.e.mu.Lock()
	fresh.e.expire(fresh.at.Add(2 * bidOutcomeDelay))
	n := len(fresh.e.sent)
	fresh.e.mu.Unlock()
	if n != 0 {
		t.Fatalf("a drop without a rival recorded %d outcomes", n)
	}
}
