package arb

import (
	"context"
	"runtime"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// On one core every short route is sized before the long route is ranked.
// The earlier rule let the long route's first quote in after one short route.
func TestShortRoutesAreSizedBeforeLongRouteRanks(t *testing.T) {
	previous := runtime.GOMAXPROCS(1)
	defer runtime.GOMAXPROCS(previous)
	var pools []pool.State
	var routes [][]domain.PoolKey
	for i := byte(0); i < 4; i++ {
		a, b := testV2State(2*i+1, 1, i+2, 1000000, 2000000), testV2State(2*i+2, i+2, 1, 1000000, 2000000)
		pools = append(pools, a, b)
		routes = append(routes, []domain.PoolKey{a.Key, b.Key})
	}
	var long []domain.PoolKey
	for i := byte(0); i < 6; i++ {
		from, to := byte(10+i), byte(11+i)
		if i == 0 {
			from = 1
		}
		if i == 5 {
			to = 1
		}
		p := testV2State(20+i, from, to, 1000000, 2000000)
		pools = append(pools, p)
		long = append(long, p.Key)
	}
	routes = append(routes, long)
	engine := mustEngine(t, explicitTable(t, pools[0].Token0, routes...), 1000, 8, v2CurveQuoter())
	view := newFakeView()
	var dirty []domain.PoolKey
	for _, p := range pools {
		view.add(p)
		dirty = append(dirty, p.Key)
	}
	ctx, cancel := context.WithCancel(t.Context())
	opts := rankedOptions(pools[0].Token0, 100000)
	opts.StreamInitial = true
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	if err := worker.Submit(SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}); err != nil {
		t.Fatal(err)
	}
	shortBeforeLong, optimized := 0, 0
	longSeen := false
	for {
		event := nextSearch(t, worker)
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		for _, q := range event.Result.Opportunities {
			if q.Cycle.Len == 6 {
				longSeen = true
			}
			if event.Stage == QuoteOptimized {
				optimized++
				if !longSeen {
					shortBeforeLong++
				}
			}
		}
		worker.Accept(event)
		releaseSearch(event.Job)
		if !event.Partial {
			break
		}
	}
	if !longSeen || shortBeforeLong != 4 || optimized != 5 {
		t.Fatalf("long route preceded short sizing, or queued sizing lost: before=%d total=%d long=%v", shortBeforeLong, optimized, longSeen)
	}
}
