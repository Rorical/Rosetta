package pipeline

import (
	"bytes"
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
	"os"
	"path/filepath"
	"testing"
)

func TestFeeCalibrationExplainsRealSlipstreamWithoutChangingPrestate(t *testing.T) {
	dir := filepath.Join("..", "..", "testdata", "golden", "op-slipstream-cycle-158960654")
	read := func(name string) []byte {
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	store := state.New()
	if err := store.LoadSnapshot(read("snapshot.json")); err != nil {
		t.Fatal(err)
	}
	before, _ := store.EncodeSnapshot()
	var record backtest.Record
	if err := json.Unmarshal(read("block.json"), &record); err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Transaction string            `json:"transaction"`
		Fees        map[string]uint32 `json:"execution_fees"`
	}
	if err := json.Unmarshal(read("expected.json"), &expected); err != nil {
		t.Fatal(err)
	}
	var refreshed []pool.State
	for _, p := range store.Pools() {
		if fee, ok := expected.Fees[p.Key.Address.Hex()]; ok {
			p.Fee = fee
			refreshed = append(refreshed, p)
		}
	}
	got, err := AuditSwaps(t.Context(), store.View(), record.Block, record.Transactions, 256, refreshed...)
	if err != nil {
		t.Fatal(err)
	}
	explained := 0
	for _, row := range got.Comparisons {
		if row.Transaction.Hex() != expected.Transaction || row.Status != "different" {
			continue
		}
		if row.Explanation == "canonical_fee_refresh_match" || row.Explanation == "canonical_fee_and_exact_output_match" {
			explained++
			if row.Requoted == nil || row.Delta.Sign() == 0 {
				t.Fatal("original evidence replaced", row)
			}
		}
	}
	if explained < 1 {
		t.Fatal("real dynamic fee discrepancy not classified", got)
	}
	after, _ := store.EncodeSnapshot()
	if !bytes.Equal(before, after) {
		t.Fatal("future fee changed historical prestate")
	}
}
