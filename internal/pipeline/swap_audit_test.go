package pipeline

import (
	"bytes"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/state"
)

func auditWords(values ...int64) []byte {
	data := make([]byte, 32*len(values))
	for index, value := range values {
		big.NewInt(value).FillBytes(data[index*32 : (index+1)*32])
	}
	return data
}

func TestSwapAuditUsesPreSyncReservesForRepeatedSwaps(t *testing.T) {
	store, _, _, _ := researchFixture(t)
	base := domain.BlockRef{Number: 10, Hash: domain.Hash{10}}
	if err := store.BeginBlock(base); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	p := store.Pools()[0]
	ref := domain.BlockRef{Number: 11, ParentHash: base.Hash, Hash: domain.Hash{11}}
	// Two real-form V2 log pairs in one receipt. Their observed amounts are
	// independent integer vectors (997 fee numerator and floor division).
	tx := ingest.Transaction{Hash: domain.Hash{1}, Successful: true, Logs: []ingest.Log{
		{Address: p.Key.Address, Topics: []domain.Hash{uniswapv2.TopicSync}, Data: auditWords(1001000, 1998008)},
		{Address: p.Key.Address, Topics: []domain.Hash{auditSwapV2, {}, {}}, Data: auditWords(1000, 0, 0, 1992)},
		{Address: p.Key.Address, Topics: []domain.Hash{uniswapv2.TopicSync}, Data: auditWords(1000501, 1999008)},
		{Address: p.Key.Address, Topics: []domain.Hash{auditSwapV2, {}, {}}, Data: auditWords(0, 1000, 499, 0)},
	}}
	before, _ := store.EncodeSnapshot()
	result, err := AuditSwaps(t.Context(), store.View(), ref, []ingest.Transaction{tx}, 8)
	if err != nil || len(result.Comparisons) != 2 {
		t.Fatal(result, err)
	}
	for _, row := range result.Comparisons {
		if row.Status != "matched" {
			t.Fatalf("wrong pre-swap state: %+v", row)
		}
	}
	after, _ := store.EncodeSnapshot()
	if !bytes.Equal(before, after) {
		t.Fatal("audit mutated live store")
	}
	limited, err := AuditSwaps(t.Context(), store.View(), ref, []ingest.Transaction{tx}, 1)
	if err != nil || len(limited.Comparisons) != 1 || !limited.Limited {
		t.Fatal("sample bound", limited, err)
	}
}

func TestSwapAuditComparesRecordedSuccessfulTransactions(t *testing.T) {
	for _, name := range []string{"op-weth-hai-158954685", "op-velodrome-cycle-158954686", "op-slipstream-cycle-158960654"} {
		t.Run(name, func(t *testing.T) {
			directory := filepath.Join("..", "..", "testdata", "golden", name)
			read := func(name string) []byte {
				raw, err := os.ReadFile(filepath.Join(directory, name))
				if err != nil {
					t.Fatal(err)
				}
				return raw
			}
			snapshot, block := "snapshot.json", "block.json"
			if name == "op-weth-hai-158954685" {
				snapshot, block = "snapshot.bin", "blocks.jsonl"
			}
			store := state.New()
			if err := store.LoadSnapshot(read(snapshot)); err != nil {
				t.Fatal(err)
			}
			var record backtest.Record
			if err := json.Unmarshal(read(block), &record); err != nil {
				t.Fatal(err)
			}
			result, err := AuditSwaps(t.Context(), store.View(), record.Block, record.Transactions, 256)
			if err != nil || len(result.Comparisons) < 2 || result.Limited {
				t.Fatal(result, err)
			}
			available, different := 0, 0
			for _, row := range result.Comparisons {
				if row.Quoted != nil {
					available++
				}
				if row.Status == "different" {
					different++
				}
				t.Logf("pool=%s status=%s delta=%v reason=%s", row.Pool.Address, row.Status, row.Delta, row.Reason)
			}
			if available == 0 {
				t.Fatal("no real receipt amounts compared")
			}
			if name == "op-velodrome-cycle-158954686" && available != len(result.Comparisons) {
				t.Fatal("Velodrome fee collector incorrectly treated as a reserve donation")
			}
			if name == "op-slipstream-cycle-158960654" && different == 0 {
				t.Fatal("missed recorded live-fee discrepancy")
			}
			if name != "op-weth-hai-158954685" {
				var expected struct {
					Transaction string            `json:"transaction"`
					Fees        map[string]uint32 `json:"execution_fees"`
				}
				if err := json.Unmarshal(read("expected.json"), &expected); err != nil {
					t.Fatal(err)
				}
				for _, p := range store.Pools() {
					if fee, ok := expected.Fees[p.Key.Address.Hex()]; ok {
						p.Fee = fee
						if err := store.Upsert(p); err != nil {
							t.Fatal(err)
						}
					}
				}
				corrected, err := AuditSwaps(t.Context(), store.View(), record.Block, record.Transactions, 256)
				if err != nil {
					t.Fatal(err)
				}
				checked := 0
				for _, row := range corrected.Comparisons {
					if row.Transaction.Hex() != expected.Transaction {
						continue
					}
					checked++
					if row.Delta == nil || new(big.Int).Abs(row.Delta).Cmp(big.NewInt(2)) > 0 {
						t.Fatalf("observed EVM fees did not reconcile real swap: %+v", row)
					}
				}
				if checked == 0 {
					t.Fatal("successful receipt not checked")
				}
			}
		})
	}
}
