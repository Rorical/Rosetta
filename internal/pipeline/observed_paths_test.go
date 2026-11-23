package pipeline

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestRecordedReceiptTeachesPathBeyondPrimaryDepth(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "golden", "op-velodrome-cycle-158954686")
	raw, err := os.ReadFile(filepath.Join(dir, "snapshot.json"))
	if err != nil {
		t.Fatal(err)
	}
	store := state.New()
	if err := store.LoadSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "block.json"))
	if err != nil {
		t.Fatal(err)
	}
	var block backtest.Record
	if err := json.Unmarshal(raw, &block); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(filepath.Join(dir, "expected.json"))
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Cycle            graph.Cycle
		TransactionIndex int `json:"transaction_index"`
	}
	if err := json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	options := TopologyOptions{Depth: 2, MaxCycles: 1, K: 1, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }}
	topology, err := BuildTopology(t.Context(), store.Pools(), options)
	if err != nil {
		t.Fatal(err)
	}
	topology.ObserveTransactions(block.Block, []ingest.Transaction{block.Transactions[expected.TransactionIndex]})
	learned := topology.Engine.ObservedCycles()
	if len(learned) != 1 || learned[0].Len != 5 || topology.Table.ContainsCycle(learned[0]) {
		t.Fatal("recorded five-hop route still depends on primary-table enumeration", len(learned))
	}
	replacement, err := ExtendTopology(t.Context(), store.Pools(), topology, nil, options)
	if err != nil || len(replacement.Engine.ObservedCycles()) != 1 {
		t.Fatal("topology replacement lost observed routes", err)
	}
}
