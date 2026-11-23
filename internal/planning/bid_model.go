package planning

import (
	"math"
	"math/big"
	"slices"
	"sort"
)

// Reasons a bid decision declines to send.
const (
	// BidNoProfit: no affordable tip has a positive expected profit.
	BidNoProfit = "no_expected_profit"
	// BidUnlikely: a positive expectation exists only at tips that win less
	// often than the minimum; such sends mostly revert, and each one also
	// keeps our other routes off its pools while it is in flight.
	BidUnlikely = "win_probability_below_minimum"
	// BidPredatory: as BidUnlikely or BidNoProfit, and the contender that
	// costs the most win probability bids above the opportunity's value.
	BidPredatory = "predatory_rival"
)

// BidAtom is one tip a contender may bid, with its probability.
type BidAtom struct {
	Tip    uint64
	Weight float64
}

// Contender is a bidder that contests this opportunity with probability
// Presence and then bids from Bids (weights summing to one). Predatory marks
// a rival that often bids above the opportunity's value.
type Contender struct {
	Name      string
	Presence  float64
	Bids      []BidAtom
	Predatory bool
}

// WinModel approximates execution success from independent rival bid
// distributions and a tip-independent early-arrival term:
//
//	P(win | t) = (1 − First) · Π_i [1 − π_i · P(b_i ≥ t)]
//
// First is the modelled chance that competition arrives too early for our
// transaction. Neither that timing label nor tie-breaking proves actual ordering.
// The bid-threshold approximation treats ties as losses. Shift recalibrates
// the product on the logit scale from our outcomes at this opportunity's size.
type WinModel struct {
	First      float64
	Contenders []Contender
	Shift      float64
}

// curve holds a contender's bids sorted by tip with suffix sums, so
// P(b ≥ t) is a binary search.
type curve struct {
	presence float64
	tips     []uint64
	tail     []float64 // tail[i] = Σ weight of bids i..end
}

func newCurve(c Contender) curve {
	bids := slices.Clone(c.Bids)
	slices.SortStableFunc(bids, func(a, b BidAtom) int {
		switch {
		case a.Tip < b.Tip:
			return -1
		case a.Tip > b.Tip:
			return 1
		}
		return 0
	})
	out := curve{presence: c.Presence, tips: make([]uint64, len(bids)), tail: make([]float64, len(bids)+1)}
	for i := len(bids) - 1; i >= 0; i-- {
		out.tips[i] = bids[i].Tip
		out.tail[i] = out.tail[i+1] + bids[i].Weight
	}
	return out
}

func (c curve) atLeast(tip uint64) float64 {
	i := sort.Search(len(c.tips), func(i int) bool { return c.tips[i] >= tip })
	return min(c.tail[i], 1)
}

func (c curve) loss(tip uint64) float64 { return c.presence * c.atLeast(tip) }

func rawWin(first float64, curves []curve, tip uint64) float64 {
	p := 1 - first
	for _, c := range curves {
		p *= 1 - c.loss(tip)
	}
	return clamp01(p)
}

// Raw is P(win | tip) before calibration.
func (m WinModel) Raw(tip uint64) float64 {
	curves := make([]curve, len(m.Contenders))
	for i, c := range m.Contenders {
		curves[i] = newCurve(c)
	}
	return rawWin(m.First, curves, tip)
}

// Calibrate shifts a raw win probability on the logit scale. A zero shift
// returns it unchanged.
func Calibrate(raw, shift float64) float64 {
	if shift == 0 {
		return raw
	}
	p := min(max(raw, 1e-3), 1-1e-3)
	return 1 / (1 + math.Exp(-(math.Log(p/(1-p)) + shift)))
}

// TipInput is one opportunity: Surplus is native wei after base costs at a
// zero tip, Gas the gas it is expected to use, Ceiling the highest tip the
// policy, fee cap, budget and surplus per unit of gas limit allow.
type TipInput struct {
	// Forecast overrides probability/cost per candidate. False excludes it.
	Forecast      func(tip uint64, raw, calibrated float64) (win float64, revertGas uint64, eligible bool)
	CandidateTips []uint64

	Surplus     *big.Int
	Gas         uint64
	Base, Extra *big.Int
	Ceiling     *big.Int
	MinWin      float64
	// RevertGas is the gas a lost auction burns. Zero means the route's swaps
	// ran before the profit check reverted it: about half of Gas.
	RevertGas uint64
	// Floor is the lowest tip considered, capped at the ceiling. It guards
	// against a win curve too flat to reward any tip.
	Floor uint64
	// SurplusAtTip returns conservative native surplus after tip-sensitive pool
	// fees, before priority gas. It must not exceed Surplus (the zero-tip bound).
	// False excludes a candidate that cannot satisfy the execution budget.
	SurplusAtTip func(uint64) (*big.Int, bool)
	// MaxEvaluations bounds local pool requotes; zero uses 32.
	MaxEvaluations int
}

// TipOption is a tip with its calibrated and raw win probability and its
// expected native profit per attempt.
type TipOption struct {
	Tip         *big.Int
	Win, WinRaw float64
	Expected    *big.Int
}

// TipChoice is the chosen tip. When Reason is set the opportunity should not
// be sent and TipOption is the best unconstrained tip, for the record.
// Limiter names the contender that costs the most win probability at the
// highest affordable tip. Probe, set only with BidPredatory, is a zero tip
// whose expectation is not negative: a cheap way to notice that a predatory
// rival has stopped.
type TipChoice struct {
	TipOption
	Reason      string
	Limiter     string
	Evaluations int
	Truncated   bool
	Probe       *TipOption
}

// ChooseTip maximizes
//
//	E(t) = P(t)·(surplus − t·gas) − (1 − P(t))·(lost·(base + t) + extra)
//
// over the floor and every contender bid plus one wei above it up to the
// ceiling, among tips whose calibrated P(t) is at least MinWin. A losing
// transaction pays lost gas and the L1 data charge: RevertGas when the executor rejects moved pools
// before swapping, otherwise roughly half its gas. On a tie the lower tip is
// kept.
func ChooseTip(m WinModel, in TipInput) TipChoice {
	curves := make([]curve, len(m.Contenders))
	for i, c := range m.Contenders {
		curves[i] = newCurve(c)
	}
	ceiling := uint64(math.MaxUint64 - 1)
	if in.Ceiling.IsUint64() {
		ceiling = in.Ceiling.Uint64()
	}
	if in.Ceiling.Sign() < 0 {
		ceiling = 0
	}
	floor := min(in.Floor, ceiling)
	candidates := []uint64{floor}
	for _, tip := range in.CandidateTips {
		if tip >= floor && tip <= ceiling {
			candidates = append(candidates, tip)
		}
	}
	for _, c := range curves {
		for _, tip := range c.tips {
			if tip < ceiling && tip+1 > floor {
				candidates = append(candidates, tip+1)
			}
		}
	}
	slices.Sort(candidates)
	candidates = slices.Compact(candidates)

	surplus, _ := new(big.Float).SetInt(in.Surplus).Float64()
	base, _ := new(big.Float).SetInt(in.Base).Float64()
	extra, _ := new(big.Float).SetInt(in.Extra).Float64()
	gas := float64(in.Gas)
	lost := gas / 2
	if in.RevertGas != 0 {
		lost = float64(in.RevertGas)
	}
	// Candidate arithmetic needs no big integers. Materialize only the chosen
	// option (and optional probe), avoiding allocations at every CDF breakpoint.
	type evaluatedTip struct {
		tip             uint64
		raw, win, value float64
	}
	option := func(tip uint64) evaluatedTip {
		raw := rawWin(m.First, curves, tip)
		win := Calibrate(raw, m.Shift)
		candidateLost := lost
		if in.Forecast != nil {
			probability, units, eligible := in.Forecast(tip, raw, win)
			if !eligible || !(probability >= 0 && probability <= 1) || units == 0 {
				return evaluatedTip{tip, raw, 0, math.Inf(-1)}
			}
			win, candidateLost = probability, float64(units)
		}
		t := float64(tip)
		value := win*(surplus-t*gas) - (1-win)*(candidateLost*(base+t)+extra)
		return evaluatedTip{tip, raw, win, value}
	}
	materialize := func(o evaluatedTip) TipOption {
		expected, _ := big.NewFloat(o.value).Int(nil)
		return TipOption{Tip: new(big.Int).SetUint64(o.tip), Win: o.win, WinRaw: o.raw, Expected: expected}
	}
	var best, likely evaluatedTip
	evaluations, truncated := 0, false
	var options []evaluatedTip
	if in.SurplusAtTip != nil {
		options = make([]evaluatedTip, 0, len(candidates))
		for _, tip := range candidates {
			options = append(options, option(tip))
		}
		// The untaxed EV bounds every repriced EV. Visit the most promising
		// candidates first; break only when all remaining bounds are dominated.
		slices.SortStableFunc(options, func(a, b evaluatedTip) int {
			if a.tip == b.tip {
				return 0
			}
			// Always evaluate the cheapest admissible alternative before the cap.
			if a.tip == floor {
				return -1
			}
			if b.tip == floor {
				return 1
			}
			if a.value > b.value {
				return -1
			}
			if a.value < b.value {
				return 1
			}
			return 0
		})
	}
	bestValue, likelyValue := math.Inf(-1), math.Inf(-1)
	for i, tip := range candidates {
		var upper evaluatedTip
		if options != nil {
			upper = options[i]
		} else {
			upper = option(tip)
		}
		o := upper
		if math.IsInf(o.value, -1) {
			continue
		}
		if in.SurplusAtTip != nil {
			if likelyValue > 0 && upper.value < likelyValue {
				break
			}
			limit := in.MaxEvaluations
			if limit <= 0 {
				limit = 32
			}
			if evaluations == limit {
				truncated = true
				break
			}
			evaluations++
			priced, ok := in.SurplusAtTip(o.tip)
			if !ok || priced == nil || priced.Cmp(in.Surplus) > 0 {
				continue
			}
			value, _ := new(big.Float).SetInt(priced).Float64()
			o.value -= o.win * (surplus - value)
		}
		if o.value > bestValue || o.value == bestValue && o.tip < best.tip {
			best, bestValue = o, o.value
		}
		if o.win >= in.MinWin && (o.value > likelyValue || o.value == likelyValue && o.tip < likely.tip) {
			likely, likelyValue = o, o.value
		}
	}
	if likelyValue > 0 {
		return TipChoice{TipOption: materialize(likely), Evaluations: evaluations, Truncated: truncated}
	}
	if math.IsInf(bestValue, -1) {
		return TipChoice{TipOption: TipOption{Tip: new(big.Int), Expected: new(big.Int)}, Reason: BidNoProfit, Evaluations: evaluations, Truncated: truncated}
	}
	choice := TipChoice{TipOption: materialize(best), Reason: BidNoProfit, Evaluations: evaluations, Truncated: truncated}
	if bestValue > 0 {
		choice.Reason = BidUnlikely
	}
	// Who keeps the win probability down even at the highest affordable tip.
	top := candidates[len(candidates)-1]
	limiter, worst := -1, m.First
	for i, c := range curves {
		if loss := c.loss(top); loss > worst {
			limiter, worst = i, loss
		}
	}
	if limiter >= 0 {
		choice.Limiter = m.Contenders[limiter].Name
		if m.Contenders[limiter].Predatory {
			choice.Reason = BidPredatory
		}
	}
	if choice.Reason == BidPredatory && in.SurplusAtTip == nil && floor == 0 {
		if probe := option(0); probe.value >= 0 {
			value := materialize(probe)
			choice.Probe = &value
		}
	}
	return choice
}

func clamp01(p float64) float64 { return min(max(p, 0), 1) }
