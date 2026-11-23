package remote

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
)

type reactiveSink struct{ r *runtime }

func (s reactiveSink) Consume(ctx context.Context, event arb.SearchEvent) error {
	return s.r.offerReactive(ctx, event)
}

// A search whose discovery outlives a frame that did not touch its route is
// carried to that frame and admitted; a later change to the route still
// invalidates the admitted intent before send.
func TestReactiveAdmitsSearchCarriedPastUnrelatedFrame(t *testing.T) {
	r, fixture := reactiveFixture(t)
	route := fixture.Result.Opportunities[0].Cycle
	ctx, cancel := context.WithCancel(t.Context())
	w, err := arb.NewWorker(ctx, r.topology.Engine, arb.RankedOptions{StreamInitial: true, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, token == route.Start
	}, SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-w.Done() })
	search := pipeline.NewSearchScheduler(w, 1, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	first := arb.SearchJob{Incremental: true, View: r.store.View(), Dirty: route.Pools[:1], GraphVersion: 1, ReceivedAt: time.Now(), Version: r.version}
	first.Prepare = func(ctx context.Context, view arb.View, _ []domain.PoolKey) (arb.View, []arb.QuoteIssue, error) {
		close(entered)
		select {
		case <-release:
			return view, nil, nil
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		}
	}
	if err := search.Submit(first, nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	second := first
	second.Prepare = nil
	second.Dirty = []domain.PoolKey{{Kind: domain.PoolKindV2, Address: addressAt(99)}}
	second.Version.TxIndex = 1
	r.version = second.Version
	if err := search.Submit(second, nil); err != nil {
		t.Fatal(err)
	}
	close(release)
	// Stop at the first search's final event: the second frame's search, which
	// also covers the accumulated route pool, runs only after it.
	for {
		var event arb.SearchEvent
		select {
		case event = <-w.Results():
		case <-time.After(5 * time.Second):
			t.Fatal("search did not finish")
		}
		origin, final := event.Job.Version, !event.Partial
		if err := search.Process(ctx, event, r, nil, nil, reactiveSink{r}); err != nil {
			t.Fatal(err)
		}
		if final && origin != first.Version {
			t.Fatal("the unrelated frame canceled the first search")
		}
		if final {
			break
		}
	}
	if len(r.reactive.jobs) != 1 {
		t.Fatal("route from the older frame was not admitted", r.reactive.takeSkips())
	}
	<-r.reactive.jobs
	job := r.reactive.takeJob(r.reactive.primary())
	if !job.current() {
		t.Fatal("unchanged route rejected")
	}
	r.reactive.touchPools(route.Pools[1:])
	if job.current() {
		t.Fatal("route change after admission ignored")
	}
}
