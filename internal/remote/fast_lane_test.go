package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestFastLaneQueuesReactiveIntentWithoutTheSearchWorker(t *testing.T) {
	r, event := reactiveFixture(t)
	start := event.Result.Opportunities[0].Cycle.Start
	r.searchOptions = arb.RankedOptions{SwapFee: dex.SwapFee, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, token == start
	}}
	if r.topology.Fast == nil {
		t.Fatal("topology has no fast lane")
	}
	r.fastLane(t.Context(), event.Job.View, event.Result.Opportunities[0].Cycle.Pools, event.Job.Version, time.Now())
	r.reactive.offerMu.Lock()
	idle := r.reactive.fastSlot == nil
	r.reactive.offerMu.Unlock()
	if !idle {
		t.Fatal("fast lane ran while switched off")
	}
	r.reactive.settings.document.FastLane = true
	r.fastLane(t.Context(), event.Job.View, event.Result.Opportunities[0].Cycle.Pools, event.Job.Version, time.Now())
	// The build worker is not running in this fixture; build the queued offer here.
	r.reactive.offerMu.Lock()
	offer := r.reactive.fastSlot
	r.reactive.fastSlot = nil
	r.reactive.offerMu.Unlock()
	if offer == nil {
		t.Fatal("fast lane queued nothing", r.reactive.takeSkips())
	}
	r.buildReactive(t.Context(), offer)
	if len(r.reactive.jobs) != 1 {
		t.Fatal("fast lane did not reach the reactive build", r.reactive.takeSkips())
	}
	<-r.reactive.jobs
	job := r.reactive.takeJob(r.reactive.primary())
	if job.timing.Lane != "fast" || job.timing.QuotedNS == 0 || job.intent.Route.Amount.Sign() <= 0 {
		t.Fatal("fast intent lacks its lane or quote time", job.timing)
	}
	// A standard-source quote has no fast-frame pickup; the quote start still
	// brackets the lane's own quoting.
	if job.timing.LaneStartNS == 0 || job.timing.LaneStartNS > job.timing.QuotedNS || job.timing.DequeuedNS != 0 {
		t.Fatal("fast lane stamps out of order", job.timing)
	}
}
