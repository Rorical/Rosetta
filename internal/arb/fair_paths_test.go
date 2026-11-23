package arb

import (
	"context"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestExactAdmissionInterleavesAssetsWithoutDroppingRanks(t *testing.T) {
	var candidates []rankedCycle
	for asset, count := range []int{32, 3, 1} {
		for rank := range count {
			candidates = append(candidates, rankedCycle{cycle: graph.Cycle{Start: domain.Address{byte(asset + 1)}}, path: asset*100 + rank})
		}
	}
	before := append([]rankedCycle(nil), candidates...)
	ordered := interleaveAssets(candidates)
	want := []int{0, 100, 200, 1, 101, 2, 102}
	for rank := 3; rank < 32; rank++ {
		want = append(want, rank)
	}
	var got []int
	for _, candidate := range ordered {
		got = append(got, candidate.path)
	}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(candidates, before) {
		t.Fatal("asset fairness changed ranks, dropped work or mutated selection", got)
	}
}

func TestSingleCoreStreamsSecondAssetBeforeFirstAssetsRemainingPaths(t *testing.T) {
	states := []pool.State{
		testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000),
		testV2State(3, 1, 3, 1000000, 1500000), testV2State(4, 3, 1, 1000000, 1500000),
		testV2State(5, 9, 10, 1000000, 2000000), testV2State(6, 10, 9, 1000000, 2000000),
	}
	g, err := graph.Build(states)
	if err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTable(g, 2, 100)
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, table, 1000, 2, v2CurveQuoter())
	opts := rankedOptions(states[0].Token0, 100000)
	originalBounds := opts.Bounds
	opts.Bounds = func(token domain.Address) (InputBounds, bool) {
		bounds, _ := originalBounds(states[0].Token0)
		return bounds, token == states[0].Token0 || token == states[4].Token0
	}
	ctx, cancel := context.WithCancel(t.Context())
	w, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-w.Done() })
	w.mu.Lock()
	w.cpus = newSearchCPUsOf(1)
	w.mu.Unlock()
	view := newFakeView()
	var dirty []domain.PoolKey
	for _, p := range states {
		view.add(p)
		dirty = append(dirty, p.Key)
	}
	job := SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	var assets []domain.Address
	for {
		event := nextSearch(t, w)
		if event.Err != nil || !w.Rebase(&event) || !w.Accept(event) {
			t.Fatal("fair worker", event.Err)
		}
		for _, quote := range event.Result.Opportunities {
			assets = append(assets, quote.Cycle.Start)
		}
		releaseSearch(event.Job)
		if !event.Partial {
			break
		}
	}
	want := []domain.Address{states[0].Token0, states[4].Token0, states[0].Token0}
	if !reflect.DeepEqual(assets, want) {
		t.Fatal("asset was delayed behind another asset's lower ranks", assets)
	}
}
