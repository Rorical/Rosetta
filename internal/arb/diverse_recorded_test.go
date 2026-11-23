package arb

import (
	"encoding/json"
	"math/big"
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

// The captured long route has a positive virtual-reserve estimate but loses
// at its probe amount. A second short route must survive to exact evaluation.
func TestRecordedDepthReservoirRetainsPositiveShortRoute(t *testing.T) {
	raw, err := os.ReadFile("testdata/op_rank_depth_158986419.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Known struct {
			Cycle graph.Cycle
			Input *big.Int
		}
		Pools     []pool.State
		Deep      []state.DeepDelta
		Cycles    []graph.Cycle
		Misranked graph.Cycle `json:"misranked_cycle"`
		Input     *big.Int    `json:"misranked_input"`
		Net       *big.Int    `json:"misranked_net"`
	}
	if err = json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	store := state.New()
	for _, p := range fixture.Pools {
		if err = store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range fixture.Deep {
		if err = store.SetDeep(d.Key, d.State); err != nil {
			t.Fatal(err)
		}
	}
	table, err := graph.BuildTableFromCycles(fixture.Cycles)
	if err != nil {
		t.Fatal(err)
	}
	quoter, err := simulate.NewQuoter(dex.NewAdapter)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(table, fixedNotional(30000000000000000), 32, quoter)
	if err != nil {
		t.Fatal(err)
	}
	opts := RankedOptions{Diverse: true, Model: dex.RankingModel, SwapFee: dex.SwapFee, Bounds: func(token domain.Address) (InputBounds, bool) {
		return InputBounds{Min: big.NewInt(1000000000000), Max: big.NewInt(500000000000000000)}, token == fixture.Known.Cycle.Start
	}}
	view := store.View()
	for _, compiled := range []bool{false, true} {
		if compiled {
			if err = engine.PreparePaths(t.Context(), fixture.Pools); err != nil {
				t.Fatal(err)
			}
		}
		selection, err := engine.Rank(t.Context(), view, view.PoolKeys(), opts)
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, c := range selection.candidates {
			if sameRankedRoute(c, rankedCycle{cycle: fixture.Known.Cycle}) {
				found = true
			}
		}
		if !found {
			t.Fatalf("compiled=%v: recorded positive short route lost", compiled)
		}
		if len(selection.candidates) > 32 {
			t.Fatal("exact selection budget grew")
		}
	}
	positive, err := engine.QuoteCycle(t.Context(), view, fixture.Known.Cycle, fixture.Known.Input)
	if err != nil || positive.Net.Sign() <= 0 {
		t.Fatal("recorded short route is not positive", positive, err)
	}
	losing, err := engine.QuoteCycle(t.Context(), view, fixture.Misranked, fixture.Input)
	if err != nil || losing.Net.Cmp(fixture.Net) != 0 || losing.Net.Sign() >= 0 {
		t.Fatal("recorded misleading estimate no longer reproduced", losing, err)
	}
}
