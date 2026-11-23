package remote

import (
	"math"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/bidmodel"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
)

func (s *reactiveRuntime) treeScope() bidmodel.Scope {
	return bidmodel.Scope{ChainID: s.settings.planning.Chain.ChainID, CodeHash: s.settings.planning.CodeHash.Hex(), Precheck: s.settings.planning.PricePrecheck}
}

func logInteger(v *big.Int) float64 {
	if v == nil || v.Sign() <= 0 {
		return 0
	}
	f, _ := new(big.Float).SetInt(v).Float64()
	return math.Log1p(f)
}

func decisionFeatures(request planning.BidRequest, m bidModel, worth, gas, revert uint64, age int64) bidmodel.Features {
	f := bidmodel.Features{bidmodel.LogWorth: math.Log1p(float64(worth)), bidmodel.AgeMS: float64(age) / 1e6, bidmodel.LogGasLimit: math.Log1p(float64(request.Gas)),
		bidmodel.SuccessGasRatio: float64(gas) / float64(request.Gas), bidmodel.RevertGasRatio: float64(revert) / float64(request.Gas), bidmodel.Hops: float64(len(request.Route)),
		bidmodel.Funding: float64(request.Funding), bidmodel.First: m.win.First, bidmodel.Unknown: m.unknown, bidmodel.LogWindows: math.Log1p(float64(m.windows)), bidmodel.LogRouteWindows: math.Log1p(float64(m.routeWindows)),
		bidmodel.LogInput: logInteger(request.Amount), bidmodel.LogBaseFee: logInteger(request.Base), bidmodel.LogExtraFee: logInteger(request.Extra)}
	if request.Lane == "fast" {
		f[bidmodel.FastLane] = 1
	}
	for _, key := range request.Route {
		if key.Kind >= 1 && key.Kind <= 7 {
			f[bidmodel.V2+int(key.Kind)-1]++
		}
	}
	return f
}

// treeBid pins a model for the whole search. Shadow predicts only the selected
// baseline action so it adds no pool requotes or counterfactual labels.
type treeBid struct {
	model                   *bidmodel.Artifact
	features                bidmodel.Features
	mode, status            string
	worth, gasLimit, revert uint64
	ageMS, lifetimeMS       float64
}

func (s *reactiveRuntime) prepareTree(now time.Time, request planning.BidRequest, m bidModel, worth, gas, revert uint64, age int64, in *planning.TipInput) treeBid {
	t := treeBid{features: decisionFeatures(request, m, worth, gas, revert, age), mode: s.settings.document.BidTreeMode, worth: worth, gasLimit: request.Gas, revert: revert,
		ageMS: float64(age) / 1e6, lifetimeMS: float64(s.settings.planning.Lifetime) / float64(time.Millisecond)}
	if t.mode == "" {
		t.mode = "off"
	}
	if t.mode == "off" {
		t.status = "disabled"
		return t
	}
	t.model, t.status = s.trees.Snapshot(now, t.mode == "apply")
	if t.model == nil || t.mode != "apply" {
		return t
	}
	in.Forecast = func(tip uint64, raw, baseline float64) (float64, uint64, bool) {
		p, ok := t.model.Predict(t.features.AtTip(tip, worth, raw))
		if !ok {
			return baseline, revert, true
		}
		return p.Win, t.revertGas(p), t.lifetimeMS <= 0 || t.ageMS+p.DelayMS < t.lifetimeMS
	}
	// A bounded logarithmic grid supplements rival CDF breakpoints; GBDT win
	// curves need not change at rival bids. Pool requotes remain capped at 32.
	if in.Ceiling.IsUint64() {
		cap := in.Ceiling.Uint64()
		for i := 1; i <= 32; i++ {
			tip := uint64(math.Expm1(math.Log1p(float64(cap)) * float64(i) / 32))
			in.CandidateTips = append(in.CandidateTips, min(tip, cap))
		}
		in.CandidateTips = append(in.CandidateTips, cap)
	}
	return t
}

func (t treeBid) revertGas(p bidmodel.Prediction) uint64 {
	return min(t.gasLimit, max(1, uint64(math.Ceil(p.RevertRatio*float64(t.gasLimit)))))
}

func (t treeBid) record(tip uint64, raw, baseline float64) *execution.BidLearning {
	f := t.features.AtTip(tip, t.worth, raw)
	r := &execution.BidLearning{Schema: bidmodel.Schema, Features: append([]float64(nil), f[:]...), Mode: t.mode, Status: t.status, BaselineWin: baseline, BaselineRevertGas: t.revert}
	if t.model == nil {
		return r
	}
	r.ModelID = t.model.ID
	p, ok := t.model.Predict(f)
	if !ok {
		r.Status = "outside_support"
		return r
	}
	r.Status, r.Applied = "predicted", t.mode == "apply"
	r.TreeWin, r.TreeRevertGas, r.RemainingMS = p.Win, t.revertGas(p), p.DelayMS
	return r
}
