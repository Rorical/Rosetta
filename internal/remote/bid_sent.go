package remote

import (
	"math"
	"time"

	"github.com/Rorical/Rosetta/internal/planning"
)

// The window model (bid_rivals.go) measures rivals over every admitted
// opportunity. Most of those nobody takes, so a rival's presence comes out
// at a few percent, while an opportunity worth sending is almost always
// contested: the window model can predict 0.45–0.95 wins where about one
// send in ten wins, most losses going to a rival in our flashblock that tips
// one to two orders of magnitude more. Transactions in one flashblock are
// ordered by tip.
//
// Our own sends sample the opportunities we do send. Each records the lowest
// tip that would have won it, and the win probability of a tip at a worth is
// the decayed, worth-weighted share of them it beats, shrunk toward the
// window model's calibrated probability by sentPriorStrength sends. At a
// worth never sent the window model stands.
const (
	sentOutcomes = 256
	sentHalfLife = 6 * time.Hour
	// One send of weight for the window model: at three, an optimistic 0.77
	// can lift a measured 0.1 over the 0.2 minimum and send what loses.
	sentPriorStrength = 1.0
	// sentWorthBand is the worth ratio at which a send's weight falls to
	// e^-1/2, narrower than the window model's: small sends win far more
	// often than large ones, and the small wins must not carry over.
	sentWorthBand = 2.0
)

// sentOutcome is one of our settled sends priced at worth. beat is the lowest
// tip that would have won it: 0 when no rival was in our flashblock, the
// highest rival tip there when tips explain the order, just under our own tip
// for a win whose flashblock is unknown, and maxBid when tips do not explain
// a loss (to a lower or unseen bid, so no tip of ours is known to win).
type sentOutcome struct {
	at    time.Time
	worth uint64
	beat  uint64
}

// addSent records o against the rivals seen in its flashblock. The caller
// holds mu.
func (e *bidEstimator) addSent(o bidOutcome) {
	if o.worth == 0 {
		return
	}
	if o.dropped {
		e.addDropped(o)
		return
	}
	var rival uint64
	if o.positioned {
		for _, s := range e.sightings {
			if s.block == o.block && s.frame == o.frame && overlaps(s.pools, o.pools) {
				rival = max(rival, s.tip)
			}
		}
	}
	sample := sentOutcome{at: o.at, worth: o.worth, beat: maxBid}
	switch {
	case o.won && !o.positioned:
		sample.beat = max(o.tip, 1) - 1
	case o.won:
		sample.beat = min(rival, o.tip)
	case rival > o.tip:
		sample.beat = rival
	}
	e.sent = append(e.sent, sample)
	if len(e.sent) > sentOutcomes {
		e.sent = e.sent[len(e.sent)-sentOutcomes:]
	}
}

// sentForecast returns the win probability of a tip from our own sends near
// worth, given the window model's calibrated one, and the tips just above
// what those sends had to beat. The caller holds mu.
func (e *bidEstimator) sentForecast(now time.Time, worth uint64) (win func(tip uint64, model float64) float64, candidates []uint64) {
	type sample struct {
		beat   uint64
		weight float64
	}
	var near []sample
	var mass float64
	for _, o := range e.sent {
		x := math.Log(float64(o.worth)/float64(max(worth, 1))) / math.Log(sentWorthBand)
		weight := decay(now.Sub(o.at), sentHalfLife) * math.Exp(-x*x/2)
		if weight < 1e-3 {
			continue
		}
		near = append(near, sample{o.beat, weight})
		mass += weight
		if o.beat < maxBid {
			candidates = append(candidates, o.beat+1)
		}
	}
	return func(tip uint64, model float64) float64 {
		wins := sentPriorStrength * model
		for _, s := range near {
			if tip > s.beat || s.beat == 0 {
				wins += s.weight
			}
		}
		return wins / (mass + sentPriorStrength)
	}, candidates
}

// applySent makes ChooseTip price tips by sentForecast; the lost gas stays
// the estimator's. A learned tree model applied later replaces it.
func applySent(in *planning.TipInput, win func(uint64, float64) float64, candidates []uint64, revertGas uint64) {
	in.CandidateTips = append(in.CandidateTips, candidates...)
	in.Forecast = func(tip uint64, _, calibrated float64) (float64, uint64, bool) {
		return win(tip, calibrated), max(revertGas, 1), true
	}
}

// droppedBlocks is how far past its quote block a dropped send's rival is
// looked for: sends land in the quote block's second frame or in the next
// block's first (over 90% of them), a few one block later.
const droppedBlocks = 2

// addDropped records a conditional send the sequencer voided because a pool
// had moved: a loss to the highest rival tip seen on its pools from the block
// it was quoted in to droppedBlocks later. Without a receipt there is no
// frame to match, so the rival may have landed a frame before the send would
// have; its tip is still the least that a send in that frame had to beat.
// No rival seen (a plain swap moved the pool, or its sighting was not kept)
// records nothing, and a rival at or under our tip is a loss no tip explains.
// The caller holds mu.
func (e *bidEstimator) addDropped(o bidOutcome) {
	var rival uint64
	for _, s := range e.sightings {
		if s.block >= o.block && s.block <= o.block+droppedBlocks && overlaps(s.pools, o.pools) {
			rival = max(rival, s.tip)
		}
	}
	if rival == 0 {
		return
	}
	sample := sentOutcome{at: o.at, worth: o.worth, beat: maxBid}
	if rival > o.tip {
		sample.beat = rival
	}
	e.sent = append(e.sent, sample)
	if len(e.sent) > sentOutcomes {
		e.sent = e.sent[len(e.sent)-sentOutcomes:]
	}
}
