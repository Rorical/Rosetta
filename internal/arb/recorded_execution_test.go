package arb_test

import (
	"crypto/sha256"
	"encoding/hex"
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

func TestRecordedSuccessfulOPArbitrageTransactionBoundary(t *testing.T) {
	directory := filepath.Join("..", "..", "testdata", "golden", "op-weth-hai-158954685")
	read := func(name string) []byte {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(directory, name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	var manifest struct {
		Checksum map[string]string `json:"checksum"`
	}
	if err := json.Unmarshal(read("manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	for name, expected := range manifest.Checksum {
		digest := sha256.Sum256(read(name))
		if actual := "sha256:" + hex.EncodeToString(digest[:]); actual != expected {
			t.Fatalf("%s checksum differs: %s", name, actual)
		}
	}
	var expected struct {
		Cycle                graph.Cycle `json:"cycle"`
		Input                string      `json:"input"`
		Gross                string      `json:"gross_profit"`
		Before               string      `json:"gross_before_trigger"`
		After                string      `json:"gross_after_next_tx"`
		ActualInput          string      `json:"actual_input"`
		ActualGross          string      `json:"actual_weth_increase"`
		ActualQuoteTolerance string      `json:"actual_quote_tolerance_wei"`
	}
	if err := json.Unmarshal(read("expected.json"), &expected); err != nil {
		t.Fatal(err)
	}
	integer := func(text string) *big.Int {
		t.Helper()
		value, ok := new(big.Int).SetString(text, 10)
		if !ok {
			t.Fatalf("invalid fixture integer: %q", text)
		}
		return value
	}
	store := state.New()
	if err := store.LoadSnapshot(read("snapshot.bin")); err != nil {
		t.Fatal(err)
	}
	var record backtest.Record
	if err := json.Unmarshal(read("blocks.jsonl"), &record); err != nil {
		t.Fatal(err)
	}
	table, err := graph.BuildTableFromCycles([]graph.Cycle{expected.Cycle})
	if err != nil {
		t.Fatal(err)
	}
	quoter, err := simulate.NewQuoter(dex.NewAdapter)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := arb.New(table, func(domain.Address) *big.Int { return big.NewInt(1_000_000_000_000_000) }, 1, quoter)
	if err != nil {
		t.Fatal(err)
	}
	if err := engine.PreparePaths(t.Context(), store.Pools()); err != nil {
		t.Fatal(err)
	}
	quote := func(view state.View, input, gross string) {
		t.Helper()
		opportunity, err := engine.QuoteCycle(t.Context(), view, expected.Cycle, integer(input))
		if err != nil {
			t.Fatal(err)
		}
		if opportunity.Net.Cmp(integer(gross)) != 0 {
			t.Fatalf("input %s: gross=%s, want=%s", input, opportunity.Net, gross)
		}
	}
	quote(store.View(), expected.Input, expected.Before)
	branch, err := ingest.NewTransactionBranch(store)
	if err != nil {
		t.Fatal(err)
	}
	if err := branch.Begin(record.Block); err != nil {
		t.Fatal(err)
	}
	for i, transaction := range record.Transactions {
		if _, err := branch.Apply(t.Context(), transaction); err != nil {
			t.Fatal(err)
		}
		view := branch.Working().View()
		switch i {
		case 5:
			quote(view, expected.Input, expected.Before)
		case 6:
			quote(view, expected.Input, expected.Gross)
			// The successful transaction used exact-output swaps. Its rounded
			// input can differ by one wei from an exact-input quote.
			actual, err := engine.QuoteCycle(t.Context(), view, expected.Cycle, integer(expected.ActualInput))
			if err != nil {
				t.Fatal(err)
			}
			difference := new(big.Int).Sub(actual.Net, integer(expected.ActualGross))
			if difference.Abs(difference).Cmp(integer(expected.ActualQuoteTolerance)) > 0 {
				t.Fatalf("quote differs from successful transaction: gross=%s, actual=%s", actual.Net, expected.ActualGross)
			}
			options := arb.RankedOptions{SwapFee: dex.SwapFee, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
				return arb.InputBounds{Min: big.NewInt(1_000_000_000_000_000), Max: big.NewInt(500_000_000_000_000_000)}, token == expected.Cycle.Start
			}}
			result, err := engine.EvaluateRanked(t.Context(), view, view.PoolKeys(), options)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Opportunities) != 1 || result.Opportunities[0].Net.Cmp(integer(expected.Gross)) < 0 {
				t.Fatalf("ranked search lost the recorded profitable route: %+v", result)
			}
		case 7:
			quote(view, expected.Input, expected.After)
		}
	}
	// This capture ends at the decision boundary, before the rest of the block,
	// so the branch is never committed.
}
