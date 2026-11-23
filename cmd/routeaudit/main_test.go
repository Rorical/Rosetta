package main

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func auditFixture(t *testing.T) (string, string, string, domain.Address, domain.Address) {
	t.Helper()
	dir := t.TempDir()
	a, b := domain.Address{1}, domain.Address{2}
	store := state.New()
	ref := domain.BlockRef{Number: 10, Hash: domain.Hash{10}, Timestamp: 1000}
	if err := store.BeginBlock(ref); err != nil {
		t.Fatal(err)
	}
	for i := byte(10); i < 12; i++ {
		if err := store.Upsert(pool.State{Key: domain.PoolKey{Address: domain.Address{i}, Kind: domain.PoolKindV2}, Token0: a, Token1: b, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(1000000), Fee: 3000}); err != nil {
			t.Fatal(err)
		}
	}
	store.Commit()
	capturePath := filepath.Join(dir, "capture.jsonl")
	capture, err := backtest.NewCapture(capturePath, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Snapshot(store.View()); err != nil {
		t.Fatal(err)
	}
	next := domain.BlockRef{Number: 11, Hash: domain.Hash{11}, ParentHash: ref.Hash, Timestamp: 1002}
	data := make([]byte, 64)
	big.NewInt(1000000).FillBytes(data[:32])
	big.NewInt(2000000).FillBytes(data[32:])
	tx := ingest.Transaction{Hash: domain.Hash{33}, Index: 0, Successful: true, Logs: []ingest.Log{{Address: domain.Address{10}, Topics: []domain.Hash{uniswapv2.TopicSync}, Data: data}}}
	if err := capture.Block(next, []ingest.Transaction{tx}); err != nil {
		t.Fatal(err)
	}
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	casesPath := filepath.Join(dir, "cases.json")
	cases := map[string]any{"chain_id": 10, "cases": []any{map[string]any{"id": "reference", "block_hash": next.Hash.Hex(), "before_tx": 1, "start": a.Hex(), "input": "100", "tokens": []string{a.Hex(), b.Hex()}, "pools": []any{map[string]any{"address": domain.Address{10}.Hex(), "kind": "v2"}, map[string]any{"address": domain.Address{11}.Hex(), "kind": "v2"}}}}}
	writeAuditJSON(t, casesPath, cases)
	searchPath := filepath.Join(dir, "search.json")
	writeAuditJSON(t, searchPath, map[string]any{"k": 8, "max_depth": 2, "max_cycles": 100, "funding": []any{map[string]any{"token": a.Hex(), "min_input": "1", "seed_input": "100", "max_input": "10000"}}})
	return capturePath, casesPath, searchPath, a, b
}
func writeAuditJSON(t *testing.T, path string, value any) {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
}
func TestRouteAuditReplaysExactPriorReceiptPositionAndReportsCoverage(t *testing.T) {
	capture, cases, search, a, _ := auditFixture(t)
	var output bytes.Buffer
	args := []string{"-capture", capture, "-cases", cases, "-search-config", search}
	if err := runCLI(t.Context(), args, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), `"found":true`) || !strings.Contains(output.String(), `"basis"`) && !strings.Contains(output.String(), `"cost_basis":"gross_only"`) {
		t.Fatal(output.String())
	}
	coverage := filepath.Join(t.TempDir(), "coverage.json")
	writeAuditJSON(t, coverage, map[string]any{"chain_id": 10, "pools": []any{map[string]any{"address": domain.Address{10}.Hex(), "kind": "v2"}}, "tokens": []string{a.Hex()}})
	output.Reset()
	if err := runCLI(t.Context(), append(args, "-coverage", coverage), &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "missing_pool") || !strings.Contains(output.String(), "missing_token") {
		t.Fatal(output.String())
	}
}
func TestRouteAuditCostsAreExplicitAndCaptureChainCannotChange(t *testing.T) {
	capture, cases, search, a, _ := auditFixture(t)
	costs := filepath.Join(t.TempDir(), "costs.json")
	writeAuditJSON(t, costs, map[string]any{"costs": []any{map[string]any{"token": a.Hex(), "network_fee_in_token": "1000000", "loan_fixed": "0", "loan_fee_pips": 0, "safety_bps": 0, "min_net_profit": "1"}}})
	var output bytes.Buffer
	args := []string{"-capture", capture, "-cases", cases, "-search-config", search, "-research-config", costs}
	if err := runCLI(t.Context(), args, &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "costs_exceed_profit") {
		t.Fatal(output.String())
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte(`"chain_id":10`), []byte(`"chain_id":42161`), 1)
	if err := os.WriteFile(capture, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := runCLI(t.Context(), args, &output); err == nil || !strings.Contains(err.Error(), "chain differs") {
		t.Fatal(err)
	}
}

func TestRouteAuditSeparatesPersistentProfitFromTheLastTransactionTrigger(t *testing.T) {
	capture, cases, search, _, _ := auditFixture(t)
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	lines := bytes.Split(bytes.TrimSpace(raw), []byte("\n"))
	var block backtest.Record
	if err := json.Unmarshal(lines[1], &block); err != nil {
		t.Fatal(err)
	}
	// The first transaction opens an opportunity. A following transaction has
	// no pool logs, so a fresh per-transaction dirty search has no V2 trigger.
	block.Transactions = append(block.Transactions, ingest.Transaction{Hash: domain.Hash{34}, Index: 1, Successful: true})
	lines[1], err = json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(capture, bytes.Join(lines, []byte("\n")), 0600); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(cases)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatal(err)
	}
	document["cases"].([]any)[0].(map[string]any)["before_tx"] = 2
	writeAuditJSON(t, cases, document)
	var output bytes.Buffer
	if err := runCLI(t.Context(), []string{"-capture", capture, "-cases", cases, "-search-config", search}, &output); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Result backtest.Diagnostic `json:"result"`
	}
	if err := json.NewDecoder(&output).Decode(&row); err != nil {
		t.Fatal(err)
	}
	if row.Result.Gross == nil || row.Result.Gross.Sign() <= 0 || row.Result.Found || !strings.Contains(strings.Join(row.Result.Reasons, ","), "path_not_dirty") {
		t.Fatal("persistent opportunity was confused with a fresh dirty trigger", row.Result)
	}
}
