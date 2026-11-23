package remote

import (
	"maps"

	"github.com/Rorical/Rosetta/internal/arb"
)

// longTail keeps, per route, the block where its current run of profitable
// quotes began. In long-tail mode only routes still profitable on a
// state that holds the whole block after that one are sent: the head of that
// block is a race the fastest searchers win, and what they leave is
// the long tail. Only the main loop touches it.
type longTail struct {
	routes map[string]routeRun
}

type routeRun struct{ first, last uint64 }

const (
	// longTailGapBlocks: a route unseen for longer starts a new run.
	longTailGapBlocks = 5
	// longTailRoutes bounds the record; past it, routes outside a run go.
	longTailRoutes = 1 << 16
)

// observe records the profitable routes of a result quoted at block.
func (t *longTail) observe(opportunities []arb.Opportunity, block uint64) {
	if t.routes == nil {
		t.routes = make(map[string]routeRun)
	}
	for _, o := range opportunities {
		if o.Net == nil || o.Net.Sign() <= 0 {
			continue
		}
		key := arb.RouteKey(o.Cycle)
		run, ok := t.routes[key]
		if !ok || block > run.last+longTailGapBlocks {
			run.first = block
		}
		run.last = max(run.last, block)
		t.routes[key] = run
	}
	if len(t.routes) > longTailRoutes {
		maps.DeleteFunc(t.routes, func(_ string, run routeRun) bool { return run.last+longTailGapBlocks < block })
	}
}

// survivors returns the opportunities whose run began before anchor, the
// quote state's canonical block: that state already holds the whole block
// after the one where the route first quoted a profit.
func (t *longTail) survivors(opportunities []arb.Opportunity, anchor uint64) []arb.Opportunity {
	var kept []arb.Opportunity
	for _, o := range opportunities {
		if run, ok := t.routes[arb.RouteKey(o.Cycle)]; ok && anchor >= run.first+1 {
			kept = append(kept, o)
		}
	}
	return kept
}
