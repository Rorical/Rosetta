package remote

import (
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
)

// bidDecision prices a priority fee from the competition model. It returns
// nil until enough windows have closed; the share-of-surplus tip applies then.
func (s *reactiveRuntime) bidDecision(request planning.BidRequest) *execution.BidDecision {
	return s.priceBid(time.Now(), request)
}
func (s *reactiveRuntime) bidDecisionAt(now time.Time, route []domain.PoolKey, surplus *big.Int, gas uint64, base, extra, ceiling *big.Int) *execution.BidDecision {
	return s.priceBid(now, planning.BidRequest{Route: route, Surplus: surplus, Gas: gas, Base: base, Extra: extra, Ceiling: ceiling})
}

// priceBid prices the tip on the gas the transaction is expected to use;
// the priority fee is charged on gas used, so pricing it on the limit would
// overstate the cost of outbidding a rival. The opportunity's worth is the
// surplus per unit of that gas, the most any bidder could pay for it.
func (s *reactiveRuntime) priceBid(now time.Time, request planning.BidRequest) *execution.BidDecision {
	route, surplus, gasLimit, base, extra, ceiling := request.Route, request.Surplus, request.Gas, request.Base, request.Extra, request.Ceiling
	gas := s.bids.routeExpectedGas(route, request.Funding, gasLimit)
	worth := rivalTip(new(big.Int).Quo(surplus, new(big.Int).SetUint64(gas)))
	e := s.bids
	e.mu.Lock()
	e.expire(now)
	window := e.price(route, worth, now)
	if len(e.windows) < s.settings.document.BidMinSamples {
		e.mu.Unlock()
		return nil
	}
	m := e.model(now, route, worth)
	if s.settings.planning.Ordering == domain.OrderingArrival {
		m.win = arrivalWinModel(m.win)
	}
	ageNS := int64(0)
	if !request.ReceivedAt.IsZero() && !now.Before(request.ReceivedAt) {
		ageNS = now.Sub(request.ReceivedAt).Nanoseconds()
	}
	m.win.Shift = e.calibration.shiftAt(worth, ageNS, request.Lane)
	revertGas := e.expectedRevertGasLocked(gasLimit, gas, s.settings.planning.PricePrecheck, request.LossGas, request.Conditional)
	sentWin, sentTips := e.sentForecast(now, worth)
	contestFloor := e.contestFloor(now, route, worth)
	intercept, slope, center := e.calibration.intercept, e.calibration.slope, e.calibration.centerWorth()
	e.mu.Unlock() // Local pool quotes must never block incoming feed observations.
	// Expected success pays the current base fee on estimated gas; the
	// surplus excludes the reserve, so the difference comes back. The reserved
	// minimum profit stays withheld. OP's original reserve was twice the base
	// fee on the whole limit; Arbitrum reserves 1.1 times on expected gas, and
	// crediting the old refund there overstated a surplus about twelvefold.
	reserved := request.Reserved
	if reserved == nil {
		reserved = new(big.Int).Mul(base, new(big.Int).SetUint64(2*gasLimit))
	}
	refund := new(big.Int).Sub(reserved, new(big.Int).Mul(base, new(big.Int).SetUint64(gas)))
	if refund.Sign() < 0 {
		refund.SetInt64(0)
	}
	expectedSurplus := new(big.Int).Add(surplus, refund)
	input := planning.TipInput{Surplus: expectedSurplus, Gas: gas, Base: base, Extra: extra, Ceiling: ceiling, MinWin: s.settings.minWin, RevertGas: revertGas}
	if request.MinimumTip != nil {
		if request.MinimumTip.Cmp(ceiling) > 0 {
			return &execution.BidDecision{Applied: s.settings.document.BidModel == "apply", Reason: planning.BidNoProfit}
		}
		input.Floor = rivalTip(request.MinimumTip)
	}
	// A lost race that costs nothing: at least freeLossShare of the worth.
	if request.Conditional && request.LossGas == planning.FreeLoss {
		num, den := freeLossShare(surplus)
		share := worth / den * num
		if c := rivalTip(ceiling); share > c {
			share = c
		}
		input.Floor = max(input.Floor, share)
	}
	// A paid loss on a route worth bidding for by value.
	if request.ShareTip != nil {
		input.Floor = max(input.Floor, rivalTip(request.ShareTip))
	}
	// What recent contests on these pools paid, when we can pay it.
	if contestFloor > input.Floor && new(big.Int).SetUint64(contestFloor).Cmp(ceiling) <= 0 {
		input.Floor = contestFloor
	} else {
		contestFloor = 0
	}
	if request.SurplusAtTip != nil {
		input.SurplusAtTip = func(tip uint64) (*big.Int, bool) {
			value, ok := request.SurplusAtTip(tip)
			if !ok {
				return nil, false
			}
			return new(big.Int).Add(value, refund), true
		}
	}
	applySent(&input, sentWin, sentTips, revertGas)
	tree := s.prepareTree(now, request, m, worth, gas, revertGas, ageNS, &input)
	choice := planning.ChooseTip(m.win, input)
	decision := &execution.BidDecision{Applied: s.settings.document.BidModel == "apply", Reason: choice.Reason, First: m.win.First, Unknown: m.unknown,
		Shift: m.win.Shift, ShiftIntercept: intercept, ShiftSlope: slope, ShiftCenter: center,
		CostVersion: "joint-v1", DecisionNS: now.UnixNano(), AgeNS: ageNS, Lane: request.Lane, RevertGas: revertGas, CostEvaluations: choice.Evaluations, CostTruncated: choice.Truncated,
		ContestFloor: contestFloor, Window: window, Windows: m.windows, RouteWindows: m.routeWindows, Worth: worth, Gas: gas, Cap: new(big.Int).Set(ceiling)}
	option := choice.TipOption
	if choice.Probe != nil {
		e.mu.Lock()
		if predator, err := domain.ParseAddress(choice.Limiter); err == nil && e.probe(predator, now) {
			option, decision.Probe, decision.Reason = *choice.Probe, true, ""
		}
		e.mu.Unlock()
	}
	decision.ModelTip, decision.Expected, decision.Win, decision.WinRaw = option.Tip, option.Expected, option.Win, option.WinRaw
	decision.Learning = tree.record(option.Tip.Uint64(), option.WinRaw, planning.Calibrate(option.WinRaw, m.win.Shift))
	if decision.Learning.Applied {
		decision.RevertGas = decision.Learning.TreeRevertGas
	}
	decision.Rivals = m.top(option.Tip.Uint64(), 3)
	// Declines are unlabeled diagnostics. Do not let optional training logs
	// crowd out execution evidence; count dropped records explicitly.
	if decision.Reason != "" && !s.note(map[string]any{"type": "bid_declined", "observed_at_ns": now.UnixNano(), "bid": decision}) {
		s.skip("bid_learning_record_dropped")
	}
	return decision
}

// arrivalWinModel is the win model under first come, first served ordering:
// no tip moves a transaction ahead, so any rival present on the route counts
// as there first. That is pessimistic about rivals that arrive after us; the
// calibration from our own outcomes corrects the level. Above-value bidding
// says nothing about arrival, so no rival is predatory.
func arrivalWinModel(m planning.WinModel) planning.WinModel {
	out := planning.WinModel{First: m.First, Shift: m.Shift, Contenders: make([]planning.Contender, len(m.Contenders))}
	for i, c := range m.Contenders {
		out.Contenders[i] = planning.Contender{Name: c.Name, Presence: c.Presence, Bids: []planning.BidAtom{{Tip: maxBid, Weight: 1}}}
	}
	return out
}
