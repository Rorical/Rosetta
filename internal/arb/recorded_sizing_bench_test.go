package arb_test

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

// Fixed pre-competitor snapshots isolate sizing from ranking, RPC and EVM.
func BenchmarkRecordedAmountSizing(b *testing.B) {
	for _, name := range []string{"op-velodrome-cycle-158954686", "op-slipstream-cycle-158960654"} {
		b.Run(name, func(b *testing.B) {
			read := func(name string) []byte {
				raw, err := os.ReadFile(filepath.Join("..", "..", "testdata", "golden", name))
				if err != nil {
					b.Fatal(err)
				}
				return raw
			}
			dir := name
			decode := func(file string, dst any) {
				if err := json.Unmarshal(read(filepath.Join(dir, file)), dst); err != nil {
					b.Fatal(err)
				}
			}
			var expected struct {
				Cycle            graph.Cycle
				Input            string
				TransactionIndex int `json:"transaction_index"`
			}
			decode("expected.json", &expected)
			var block backtest.Record
			decode("block.json", &block)
			store := state.New()
			if err := store.LoadSnapshot(read(filepath.Join(dir, "snapshot.json"))); err != nil {
				b.Fatal(err)
			}
			branch, err := ingest.NewTransactionBranch(store)
			if err != nil {
				b.Fatal(err)
			}
			if err := branch.Begin(block.Block); err != nil {
				b.Fatal(err)
			}
			for _, tx := range block.Transactions {
				if int(tx.Index) >= expected.TransactionIndex {
					break
				}
				if _, err := branch.Apply(b.Context(), tx); err != nil {
					b.Fatal(err)
				}
			}
			table, err := graph.BuildTableFromCycles([]graph.Cycle{expected.Cycle})
			if err != nil {
				b.Fatal(err)
			}
			quoter, err := simulate.NewQuoter(dex.NewAdapter)
			if err != nil {
				b.Fatal(err)
			}
			input, ok := new(big.Int).SetString(expected.Input, 10)
			if !ok {
				b.Fatal("fixture input")
			}
			engine, err := arb.New(table, func(domain.Address) *big.Int { return new(big.Int).Set(input) }, 1, quoter)
			if err != nil {
				b.Fatal(err)
			}
			view := branch.Working().View()
			selection, err := engine.Rank(b.Context(), view, expected.Cycle.Pools, arb.RankedOptions{SwapFee: dex.SwapFee, Model: dex.RankingModel, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
				return arb.InputBounds{Min: big.NewInt(1_000_000_000), Max: big.NewInt(100_000_000_000_000_000)}, token == expected.Cycle.Start
			}})
			if err != nil || len(selection.Pools()) == 0 {
				b.Fatal("fixture ranking", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				result, err := engine.Optimize(b.Context(), view, selection)
				if err != nil || len(result.Opportunities) != 1 {
					b.Fatal(result, err)
				}
			}
		})
	}
}
