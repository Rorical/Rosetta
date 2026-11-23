package bidreport

import (
	"bytes"
	"fmt"
	"math"
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

func summarize(predictions map[domain.Hash]prediction, outcomes map[domain.Hash]*execution.Settlement, voided map[domain.Hash]bool, diagnoses map[domain.Hash]string, evidence evidenceIndex) (Report, error) {
	r := Report{Basis: "recorded_before_send; canonical_outcomes; observed_policy_only; no_refitting; no_counterfactual_inclusion", Submitted: len(predictions), Excluded: map[string]int{}, FailureEvidence: map[string]int{}}
	groups := []map[string]*Group{{}, {}, {}, {}, {}, {}, {}}
	keys := make([]domain.Hash, 0, len(predictions))
	for key := range predictions {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b domain.Hash) int { return bytes.Compare(a[:], b[:]) })
	for _, hash := range keys {
		p, o := predictions[hash], outcomes[hash]
		if o != nil && !voided[hash] {
			if o.Intent != nil && (o.Intent.ID != p.intent.ID || o.Intent.Chain != p.intent.Chain) {
				return Report{}, fmt.Errorf("canonical outcome disagrees with recorded intent")
			}
			if o.Fees != nil && o.Fees.Total != nil && o.Fees.Total.Sign() < 0 {
				return Report{}, fmt.Errorf("negative paid fee")
			}
			r.Economics.add(p, o)
			for _, flag := range evidence.classify(p, o, diagnoses[hash]) {
				r.FailureEvidence[flag]++
			}
		}
		reason := excluded(p, o, voided[hash])
		if reason != "" {
			r.Excluded[reason]++
			continue
		}
		b := p.intent.Bid
		if !probability(b.Win) || !probability(b.WinRaw) || p.intent.Call.TipCap.Sign() < 0 {
			return Report{}, fmt.Errorf("invalid recorded bid probability or tip")
		}
		bin := min(int(b.Win*10), 9)
		worth := "unknown"
		if b.Worth > 0 {
			worth = fmt.Sprintf("1e%d", len(fmt.Sprint(b.Worth))-1)
		}
		age := int64(0)
		if p.receivedNS > 0 && p.at.UnixNano() >= p.receivedNS {
			age = p.at.UnixNano() - p.receivedNS
		}
		model := b.CostVersion
		if model == "" {
			model = "legacy"
		}
		if b.Learning != nil && b.Learning.Applied {
			model += "/gbdt/" + b.Learning.ModelID
		}
		names := []string{"all", fmt.Sprintf("%.1f-%.1f", float64(bin)/10, float64(bin+1)/10), p.lane, p.at.Format("2006-01-02T15:00Z"), worth, ageBand(age), model}
		for i, name := range names {
			g := groups[i][name]
			if g == nil {
				g = &Group{Name: name, SuccessFees: new(big.Int), RevertFees: new(big.Int)}
				groups[i][name] = g
			}
			g.add(p, o)
		}
		r.Scored++
	}
	if all := finish(groups[0]); len(all) > 0 {
		r.All = all[0]
	}
	r.Probability, r.Lane, r.Hour, r.Worth = finish(groups[1]), finish(groups[2]), finish(groups[3]), finish(groups[4])
	r.Age, r.Model = finish(groups[5]), finish(groups[6])
	return r, nil
}

func excluded(p prediction, o *execution.Settlement, voided bool) string {
	switch {
	case voided:
		return "voided"
	case o == nil:
		return "no_canonical_outcome"
	case o.Cancelled:
		return "cancelled"
	case p.intent.Bid == nil || !p.intent.Bid.Applied:
		return "no_applied_prediction"
	case p.intent.Bid.DecisionNS > p.at.UnixNano():
		return "prediction_after_submission"
	case p.intent.Bid.ModelTip == nil || p.intent.Call.TipCap == nil || p.intent.Bid.ModelTip.Cmp(p.intent.Call.TipCap) != 0:
		return "signed_tip_changed"
	}
	return ""
}

func probability(p float64) bool { return p >= 0 && p <= 1 && !math.IsNaN(p) }

func (s *Scores) add(p, y float64) {
	s.Mean += p
	s.Brier += (p - y) * (p - y)
	// Clipping only bounds the scoring penalty for a recorded 0/1 forecast.
	q := min(max(p, 1e-15), 1-1e-15)
	s.LogLoss -= y*math.Log(q) + (1-y)*math.Log1p(-q)
}

func (g *Group) add(p prediction, o *execution.Settlement) {
	b := p.intent.Bid
	g.Economics.add(p, o)
	g.Count++
	y := 0.0
	if o.Success {
		g.Success++
		y = 1
	}
	g.Raw.add(b.WinRaw, y)
	g.Calibrated.add(b.Win, y)
	if o.FeeStatus != "" || o.Fees == nil || o.Fees.Total == nil {
		return
	}
	g.Paid++
	if !o.Success {
		g.RevertFees.Add(g.RevertFees, o.Fees.Total)
		if b.RevertGas > 0 && o.Fees.GasUsed > 0 {
			g.RevertGasSamples++
			g.RevertGasRatioMean += float64(o.Fees.GasUsed) / float64(b.RevertGas)
		}
		return
	}
	g.SuccessFees.Add(g.SuccessFees, o.Fees.Total)
	if b.Gas > 0 && o.Fees.GasUsed > 0 {
		g.SuccessGasSamples++
		g.SuccessGasRatioMean += float64(o.Fees.GasUsed) / float64(b.Gas)
	}
}

func finish(groups map[string]*Group) []Group {
	out := make([]Group, 0, len(groups))
	for _, value := range groups {
		g := *value
		n := float64(g.Count)
		g.Observed = float64(g.Success) / n
		for _, s := range []*Scores{&g.Raw, &g.Calibrated} {
			s.Mean /= n
			s.Brier /= n
			s.LogLoss /= n
		}
		if g.RevertGasSamples > 0 {
			g.RevertGasRatioMean /= float64(g.RevertGasSamples)
		}
		if g.SuccessGasSamples > 0 {
			g.SuccessGasRatioMean /= float64(g.SuccessGasSamples)
		}
		out = append(out, g)
	}
	slices.SortFunc(out, func(a, b Group) int { return bytes.Compare([]byte(a.Name), []byte(b.Name)) })
	return out
}
