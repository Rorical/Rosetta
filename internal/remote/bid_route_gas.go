package remote

import (
	"encoding/hex"
	"slices"
	"strconv"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/planning"
)

// A route's gas depends on its pools' kinds, the ticks it crosses and its
// funding far more than on its gas limit, which only scales with length.
// A route whose limit-ratio estimate is 312k may use 236k, and its tip per gas
// is then priced a quarter lower than the surplus allows. The history is
// saved with the bid history (bid_store.go): a restart that lost it would
// price every route by the limit ratio until routes settled again.
const (
	routeGasSamples = 16   // recent successes kept per route
	routeGasMin     = 3    // samples of the same kinds before their median applies; of a route, before its 75th percentile replaces its largest
	routeGasRoutes  = 4096 // distinct routes kept before the history restarts
)

type routeGasHistory struct {
	exact map[string][]uint64 // pools in order, with funding
	shape map[string][]uint64 // pool kinds in order, with funding
}

// routeGasKeys formats the two history keys of a route: "<funding>" followed
// by "|<kind>:<address hex>:<pool id hex>" per pool, and by "|<kind>" per pool.
// The bid store keeps the keys as written; a changed format only forgets the
// saved history. The estimate runs for every candidate of every build and the keys are
// appended by hand: formatting them with fmt took about half of the build
// loop's CPU.
func routeGasKeys(route []domain.PoolKey, funding execution.Funding) (exact, shape string) {
	e := make([]byte, 0, 4+len(route)*112)
	s := make([]byte, 0, 4+len(route)*4)
	e = strconv.AppendInt(e, int64(funding), 10)
	s = strconv.AppendInt(s, int64(funding), 10)
	for _, key := range route {
		e = append(e, '|')
		e = strconv.AppendInt(e, int64(key.Kind), 10)
		e = append(e, ':')
		e = hex.AppendEncode(e, key.Address[:])
		e = append(e, ':')
		e = hex.AppendEncode(e, key.PoolID[:])
		s = append(s, '|')
		s = strconv.AppendInt(s, int64(key.Kind), 10)
	}
	return string(e), string(s)
}

func appendGasSample(samples []uint64, used uint64) []uint64 {
	samples = append(samples, used)
	if len(samples) > routeGasSamples {
		samples = samples[len(samples)-routeGasSamples:]
	}
	return samples
}

func planPools(plan execution.ExecutorPlan) []domain.PoolKey {
	pools := make([]domain.PoolKey, len(plan.Steps))
	for i, step := range plan.Steps {
		pools[i] = step.Pool
	}
	return pools
}

// observeRouteGas records the gas a successful, non-cancelled receipt used.
func (e *bidEstimator) observeRouteGas(route []domain.PoolKey, funding execution.Funding, used uint64) {
	if len(route) == 0 || used == 0 {
		return
	}
	exact, shape := routeGasKeys(route, funding)
	e.mu.Lock()
	defer e.mu.Unlock()
	h := &e.routeGas
	if h.exact == nil || len(h.exact) >= routeGasRoutes {
		h.exact = map[string][]uint64{}
	}
	if h.shape == nil || len(h.shape) >= routeGasRoutes {
		h.shape = map[string][]uint64{}
	}
	h.exact[exact] = appendGasSample(h.exact[exact], used)
	h.shape[shape] = appendGasSample(h.shape[shape], used)
}

// routeExpectedGas is the route's own recent gas (the largest of one or two
// samples, the 75th percentile of more), then the median of routes with the
// same kinds and funding, then the limit ratio of expectedGas; never above
// the limit. Routes of the same kinds differ by the ticks they cross and the
// tokens they move (two-hop V3 routes typically use 205k–256k, a few
// 450k–480k), so their upper quartile prices an unmeasured route as the
// dearest of them, and their median misprices a route that is one of the
// dear ones: its own first measurement replaces it (a three-hop route priced
// at 342k by its kinds can use 485k and win at a loss).
func (e *bidEstimator) routeExpectedGas(route []domain.PoolKey, funding execution.Funding, limit uint64) uint64 {
	exact, shape := routeGasKeys(route, funding)
	e.mu.Lock()
	samples, own := e.routeGas.exact[exact], true
	if len(samples) == 0 {
		samples, own = e.routeGas.shape[shape], false
	}
	// A route with MetricOmm or wrap legs whose kinds were never measured:
	// the same route over V3 pools plus metricLegExtraGas a MetricOmm leg,
	// without its wrap legs plus planning.WrapLegGas each.
	var extra uint64
	if !own && len(samples) < routeGasMin {
		if as, added := unmeasuredPrior(route); added != 0 {
			_, shape = routeGasKeys(as, funding)
			samples, extra = e.routeGas.shape[shape], added
		}
	}
	samples = slices.Clone(samples)
	e.mu.Unlock()
	if !own && len(samples) < routeGasMin {
		return e.expectedGas(limit)
	}
	slices.Sort(samples)
	at := len(samples) / 2
	if own {
		at = len(samples) * 3 / 4
		if len(samples) < routeGasMin {
			at = len(samples) - 1
		}
	}
	return min(limit, max(1, samples[at]+extra))
}

// metricLegExtraGas is what a MetricOmm swap costs over a V3 swap: 220,508
// gas for the pool call with its three extensions and the oracle read in a
// traced route, against about 120,000 for a V3 swap.
const metricLegExtraGas = 100_000

// unmeasuredPrior is the route whose measured gas stands in for route's:
// its MetricOmm pools relabelled as V3 and its wrap legs left out, with the
// gas those legs add. Zero gas means route has neither.
func unmeasuredPrior(route []domain.PoolKey) ([]domain.PoolKey, uint64) {
	var added uint64
	for _, key := range route {
		switch key.Kind {
		case domain.PoolKindMetricOmm:
			added += metricLegExtraGas
		case domain.PoolKindWrap:
			added += planning.WrapLegGas
		}
	}
	if added == 0 {
		return nil, 0
	}
	as := make([]domain.PoolKey, 0, len(route))
	for _, key := range route {
		switch key.Kind {
		case domain.PoolKindMetricOmm:
			key.Kind = domain.PoolKindV3
		case domain.PoolKindWrap:
			continue
		}
		as = append(as, key)
	}
	return as, added
}

// storedRouteGas is the route gas history in the bid store.
type storedRouteGas struct {
	Exact map[string][]uint64 `json:"exact,omitempty"`
	Shape map[string][]uint64 `json:"shape,omitempty"`
}

// stored copies the history. The caller holds mu.
func (h *routeGasHistory) stored() *storedRouteGas {
	if len(h.exact) == 0 && len(h.shape) == 0 {
		return nil
	}
	return &storedRouteGas{Exact: cloneGasSamples(h.exact), Shape: cloneGasSamples(h.shape)}
}

// restore replaces the history with a saved one. The caller holds mu.
func (h *routeGasHistory) restore(saved *storedRouteGas) {
	h.exact, h.shape = nil, nil
	if saved != nil {
		h.exact, h.shape = cloneGasSamples(saved.Exact), cloneGasSamples(saved.Shape)
	}
}

func cloneGasSamples(samples map[string][]uint64) map[string][]uint64 {
	out := make(map[string][]uint64, len(samples))
	for key, values := range samples {
		if len(out) == routeGasRoutes {
			break
		}
		out[key] = slices.Clone(values[max(0, len(values)-routeGasSamples):])
	}
	return out
}
