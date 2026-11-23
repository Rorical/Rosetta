package arb

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	poolmath "github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestCompiledCurveCachesInteriorAndSuffixSegments(t *testing.T) {
	raw, err := os.ReadFile("../../testdata/golden/op-velodrome-cycle-158954686/snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	store := state.New()
	if err := store.LoadSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile("../../testdata/golden/op-velodrome-cycle-158954686/expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected struct{ Cycle graph.Cycle }
	if err = json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTableFromCycles([]graph.Cycle{expected.Cycle})
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, table, 1000, 32, identityQuoter())
	if err := engine.PreparePaths(t.Context(), store.Pools()); err != nil {
		t.Fatal(err)
	}
	view := store.View()
	dirty := view.PoolKeys()
	opts := RankedOptions{SwapFee: dex.SwapFee, Model: dex.RankingModel, Bounds: func(token domain.Address) (InputBounds, bool) {
		return InputBounds{Min: big.NewInt(1), Max: big.NewInt(1000000000000000000)}, token == expected.Cycle.Start
	}}
	indices := engine.table.Candidates(dirty, engine.table.Len())
	chosen := -1
	for _, index := range indices {
		if len(engine.compiled.path(index)) >= 5 {
			chosen = index
			break
		}
	}
	if chosen < 0 {
		t.Fatal("recorded fixture lacks a path with a reusable suffix")
	}
	snapshot := compiledSnapshots.Get().(*compiledSnapshot)
	defer snapshot.release()
	if _, err := snapshot.prepare(t.Context(), engine.compiled, []int{chosen}, view, opts, 1); err != nil {
		t.Fatal(err)
	}
	var cache rankCache
	cache.bind(engine.compiled, 1)
	if err := cache.update(t.Context(), snapshot, 1); err != nil {
		t.Fatal(err)
	}
	work := rankWorkspaces.Get().(*rankWorkspace)
	defer work.release()
	hops := engine.compiled.path(chosen)
	work.resize(len(hops))
	for offset := range hops {
		work.prepareCurve(snapshot, hops, offset, &cache)
		var legs []poolmath.ClosedFormLeg
		for i := range hops {
			hop := hops[(i+offset)%len(hops)]
			legs = append(legs, snapshot.pools[hop/2].legs[hop%2].model)
		}
		bounds := InputBounds{Min: big.NewInt(1), Max: big.NewInt(1000000000000000000)}
		wantInput, wantNet, err := poolmath.EstimateCycle(legs, bounds.Min, bounds.Max)
		if err != nil {
			t.Fatal(err)
		}
		var input, net big.Int
		if err := work.curve.EstimateInto(&input, &net, bounds.Min, bounds.Max); err != nil {
			t.Fatal(err)
		}
		if input.Cmp(wantInput) != 0 || net.Cmp(wantNet) != 0 {
			t.Fatal("shared segments changed integer curve", offset)
		}
		suffix := segmentKey{length: len(hops) - 3}
		for i := range suffix.length {
			hop := hops[(offset+3+i)%len(hops)]
			suffix.hops[i] = hop
			suffix.revisions[i] = cache.pools[hop/2].revision
		}
		var saved poolmath.CycleCoefficients
		if !cache.segments.load(suffix, &saved) {
			t.Fatal("suffix was recomputed instead of cached", offset)
		}
	}
}
