package pipeline

import (
	"context"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// A flashblock search still in discovery when the next frame arrives. Route A
// is triggered by the first frame; the second frame changes either route B's
// pool (disjoint) or route A's trigger (overlap).
func TestFrameSearchSurvivesDisjointFrameAndCountsOutcome(t *testing.T) {
	v2 := func(address, token0, token1 byte) pool.State {
		return pool.State{Key: domain.PoolKey{Address: domain.Address{address}, Kind: domain.PoolKindV2}, Token0: domain.Address{token0}, Token1: domain.Address{token1}, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(2000000), Fee: 3000}
	}
	states := []pool.State{v2(10, 1, 2), v2(20, 2, 1), v2(30, 1, 3), v2(40, 3, 1)}
	routeA, routeB := states[0].Key, states[2].Key
	for _, test := range []struct {
		name               string
		second             domain.PoolKey
		overtaken, overlap uint64
	}{{"disjoint", routeB, 1, 0}, {"trigger", routeA, 0, 1}} {
		t.Run(test.name, func(t *testing.T) {
			topology, err := BuildTopology(t.Context(), states, TopologyOptions{Depth: 2, MaxCycles: 100, K: 4, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }})
			if err != nil {
				t.Fatal(err)
			}
			store := poolstate.New()
			for _, p := range states {
				if err := store.Upsert(p); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithCancel(t.Context())
			w, err := arb.NewWorker(ctx, topology.Engine, arb.RankedOptions{StreamInitial: true, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
				return arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, token == states[0].Token0
			}, SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cancel(); <-w.Done() })
			registry := &telemetry.Registry{}
			s := NewSearchScheduler(w, 1, registry)
			entered, release := make(chan struct{}), make(chan struct{})
			first := arb.SearchJob{Incremental: true, View: store.View(), Dirty: []domain.PoolKey{routeA}, GraphVersion: 1, ReceivedAt: time.Now(),
				Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 10}}
			first.Prepare = func(ctx context.Context, view arb.View, _ []domain.PoolKey) (arb.View, []arb.QuoteIssue, error) {
				close(entered)
				select {
				case <-release:
					return view, nil, nil
				case <-ctx.Done():
					return nil, nil, ctx.Err()
				}
			}
			if err := s.Submit(first, nil); err != nil {
				t.Fatal(err)
			}
			await(t, entered)
			second := first
			second.Prepare = nil
			second.Dirty = []domain.PoolKey{test.second}
			second.Version.TxIndex = 1
			if err := s.Submit(second, nil); err != nil {
				t.Fatal(err)
			}
			close(release)
			deliveredA := false
			sink := sinkFunc(func(_ context.Context, event arb.SearchEvent) error {
				if event.Job.Version != second.Version {
					t.Fatal("admitted result kept an obsolete frame version")
				}
				for _, quote := range event.Result.Opportunities {
					deliveredA = deliveredA || slices.Contains(quote.Cycle.Pools, states[1].Key)
				}
				return nil
			})
			cursor := cursorFunc(func(v domain.StateVersion) bool { return v == second.Version })
			for count := 0; count < 16 && s.TentativePending != (domain.StateVersion{}); count++ {
				if err := s.Process(ctx, await(t, w.Results()), cursor, nil, nil, sink); err != nil {
					t.Fatal(err)
				}
			}
			if s.TentativePending != (domain.StateVersion{}) || !deliveredA {
				t.Fatal("route A was not delivered at the newer frame")
			}
			counters := registry.Snapshot().Sources["fast"].Counters
			if counters["search_overtaken"] != test.overtaken || counters["search_canceled_overlap"] != test.overlap || counters["search_canceled"] != test.overlap {
				t.Fatal("search outcome counters", counters)
			}
		})
	}
}
