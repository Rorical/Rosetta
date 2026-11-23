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

// The writer has unapplied input when a search's candidates arrive, so it
// takes none of them, and the search itself ends acknowledged. The next frame
// changes no pool. The route must come back with it: nothing else would search
// it again, and the candidate would be lost until one of its pools moved.
func TestCandidateDroppedForQueuedInputReturnsWithNextFrame(t *testing.T) {
	v2 := func(address, token0, token1 byte) pool.State {
		return pool.State{Key: domain.PoolKey{Address: domain.Address{address}, Kind: domain.PoolKindV2}, Token0: domain.Address{token0}, Token1: domain.Address{token1}, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(2000000), Fee: 3000}
	}
	states := []pool.State{v2(10, 1, 2), v2(20, 2, 1)}
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
	first := arb.SearchJob{Incremental: true, View: store.View(), Dirty: []domain.PoolKey{states[0].Key}, GraphVersion: 1, ReceivedAt: time.Now(),
		Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 10}}
	if err := s.Submit(first, nil); err != nil {
		t.Fatal(err)
	}
	consumed := 0
	sink := sinkFunc(func(_ context.Context, event arb.SearchEvent) error {
		for _, quote := range event.Result.Opportunities {
			if slices.Contains(quote.Cycle.Pools, states[0].Key) {
				consumed++
			}
		}
		return nil
	})
	queued := cursorFunc(func(domain.StateVersion) bool { return false })
	for {
		event := await(t, w.Results())
		final := !event.Partial
		if err := s.Process(ctx, event, queued, nil, nil, sink); err != nil {
			t.Fatal(err)
		}
		if final {
			break
		}
	}
	if consumed != 0 {
		t.Fatal("writer with queued input consumed a candidate")
	}
	heartbeat := first
	heartbeat.Dirty = nil
	heartbeat.Version.TxIndex = 1
	if err := s.Submit(heartbeat, nil); err != nil {
		t.Fatal(err)
	}
	current := cursorFunc(func(v domain.StateVersion) bool { return v == heartbeat.Version })
	for {
		event := await(t, w.Results())
		final := !event.Partial
		if err := s.Process(ctx, event, current, nil, nil, sink); err != nil {
			t.Fatal(err)
		}
		if final {
			break
		}
	}
	if consumed == 0 {
		t.Fatal("route of the dropped candidate was not searched again")
	}
	snapshot := registry.Snapshot().Sources["fast"]
	if snapshot.Counters["candidates_rechecked"] == 0 || snapshot.Counters["search_completed"] != 2 || snapshot.Counters["search_frames_absorbed"] != 2 {
		t.Fatal("recheck counters", snapshot.Counters)
	}
	if snapshot.Stages["first_short_candidate"].Count != 1 || snapshot.Stages["search_short"].Count != 2 {
		t.Fatal("short-route stages", snapshot.Stages["first_short_candidate"], snapshot.Stages["search_short"])
	}
}
