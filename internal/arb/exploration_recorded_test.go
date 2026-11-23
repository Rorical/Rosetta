package arb_test

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

// Remove the recorded route from the main table entirely. Discovery must reach
// exact local quoting through the ordinary worker on both source lanes.
func TestExplorationRecoversRecordedThreeAndFiveHopMarkets(t *testing.T) {
	for _, name := range []string{"op-velodrome-cycle-158954686", "op-slipstream-cycle-158960654"} {
		t.Run(name, func(t *testing.T) {
			read := func(file string) []byte {
				raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", name, file))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			var expected struct {
				Cycle            graph.Cycle
				TransactionIndex int `json:"transaction_index"`
			}
			if err := json.Unmarshal(read("expected.json"), &expected); err != nil {
				t.Fatal(err)
			}
			store := state.New()
			if err := store.LoadSnapshot(read("snapshot.json")); err != nil {
				t.Fatal(err)
			}
			var record backtest.Record
			if err := json.Unmarshal(read("block.json"), &record); err != nil {
				t.Fatal(err)
			}
			branch, err := ingest.NewTransactionBranch(store)
			if err != nil {
				t.Fatal(err)
			}
			if err := branch.Begin(record.Block); err != nil {
				t.Fatal(err)
			}
			for _, tx := range record.Transactions {
				if int(tx.Index) >= expected.TransactionIndex {
					break
				}
				if _, err := branch.Apply(t.Context(), tx); err != nil {
					t.Fatal(err)
				}
			}
			view := branch.Working().View()
			states := store.Pools()
			g, err := graph.Build(states)
			if err != nil {
				t.Fatal(err)
			}
			table, err := graph.BuildTableFromCycles(nil)
			if err != nil {
				t.Fatal(err)
			}
			table, err = graph.PackTable(t.Context(), table, states)
			if err != nil {
				t.Fatal(err)
			}
			quoter, err := simulate.NewQuoter(dex.NewAdapter)
			if err != nil {
				t.Fatal(err)
			}
			engine, err := arb.New(table, func(domain.Address) *big.Int { return big.NewInt(1000) }, 4, quoter)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.PreparePaths(t.Context(), states); err != nil {
				t.Fatal(err)
			}
			if err := engine.EnableExploration(g, graph.DefaultExploreLimits(6)); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			worker, err := arb.NewWorker(ctx, engine, arb.RankedOptions{SwapFee: dex.SwapFee, Model: dex.RankingModel, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
				return arb.InputBounds{Min: big.NewInt(1_000_000_000), Max: big.NewInt(100_000_000_000_000_000)}, token == expected.Cycle.Start
			}})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cancel(); <-worker.Done() })
			for _, source := range []domain.StateSource{domain.SourceStandard, domain.SourceFast} {
				for _, incremental := range []bool{false, true} {
					job := arb.SearchJob{Incremental: incremental, View: view, Dirty: []domain.PoolKey{expected.Cycle.Pools[0]}, GraphVersion: 1, Version: domain.StateVersion{Source: source, Epoch: 1}}
					if err := worker.Submit(job); err != nil {
						t.Fatal(err)
					}
					found, discovered := false, false
					finished := false
					for !finished {
						select {
						case event := <-worker.Results():
							if event.Err != nil {
								t.Fatal(event.Err)
							}
							for _, o := range event.Result.Opportunities {
								if o.Cycle.Start == expected.Cycle.Start && slices.Equal(o.Cycle.Pools, expected.Cycle.Pools) && o.Net.Sign() > 0 {
									found = true
								}
							}
							if !event.Partial {
								discovered = event.Result.Exploration.Cached > 0
								finished = true
							}
							if !worker.Rebase(&event) || !worker.Accept(event) {
								t.Fatal("current historical result rejected")
							}
							if event.Job.Release != nil {
								event.Job.Release()
							}
						case <-time.After(5 * time.Second):
							t.Fatal("historical incremental search did not return")
						}
					}
					if !found || !discovered {
						t.Fatalf("recorded opportunity absent: source=%v incremental=%v", source, incremental)
					}
				}
			}
		})
	}
}
