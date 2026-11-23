package arb

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

// This OP prefix is immediately before receipt index 160. Its actual swap
// amounts independently match the model; it must remain discoverable early.
func TestRecordedLargeOPCycleStreamsCompleteQuote(t *testing.T) {
	raw, err := os.ReadFile("testdata/op_prefix_158972195_large.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		SwapInputs  []*big.Int `json:"swap_inputs"`
		SwapOutputs []*big.Int `json:"swap_outputs"`
		Pools       []pool.State
		Deep        []state.DeepDelta
		Cycle       graph.Cycle
		Input       *big.Int
		Net         string
		HintInput   string `json:"hint_input"`
		HintNet     string `json:"hint_net"`
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
	table, err := graph.BuildTableFromCycles([]graph.Cycle{fixture.Cycle})
	if err != nil {
		t.Fatal(err)
	}
	quoter, err := simulate.NewQuoter(dex.NewAdapter)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := New(table, func(domain.Address) *big.Int { return big.NewInt(10000000000000000) }, 32, quoter)
	if err != nil {
		t.Fatal(err)
	}
	if err = engine.PreparePaths(t.Context(), fixture.Pools); err != nil {
		t.Fatal(err)
	}
	view := store.View()
	actual, err := engine.QuoteCycle(t.Context(), view, fixture.Cycle, fixture.Input)
	if err != nil || actual.Net.String() != fixture.Net {
		t.Fatal("recorded competitor amount differs", actual.Net, err)
	}
	for i, leg := range actual.Legs {
		if leg.AmountIn.Cmp(fixture.SwapInputs[i]) != 0 || leg.AmountOut.Cmp(fixture.SwapOutputs[i]) != 0 {
			t.Fatal("recorded swap leg differs", i, leg)
		}
	}
	opts := RankedOptions{StreamInitial: true, Diverse: true, Bounds: func(token domain.Address) (InputBounds, bool) {
		return InputBounds{Min: big.NewInt(1000000000000000), Max: big.NewInt(500000000000000000)}, token == fixture.Cycle.Start
	}, SwapFee: dex.SwapFee, Model: dex.RankingModel}
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	if err = worker.Submit(SearchJob{Incremental: true, View: view, Dirty: view.PoolKeys(), GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 158972195, TxIndex: 159}}); err != nil {
		t.Fatal(err)
	}
	event := nextSearch(t, worker)
	defer releaseSearch(event.Job)
	if event.Stage != QuoteInitial || len(event.Result.Opportunities) != 1 {
		t.Fatal("missing initial quote", event.Stage)
	}
	quote := event.Result.Opportunities[0]
	if !slices.Equal(quote.Cycle.Pools, fixture.Cycle.Pools) || len(quote.Legs) != 3 || quote.Input.String() != fixture.HintInput || quote.Net.String() != fixture.HintNet {
		t.Fatal("incorrect early quote", quote)
	}
}
