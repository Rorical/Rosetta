package backtest

import (
	"encoding/json"
	"os"
	"testing"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestExtractReceiptCycles(t *testing.T) {
	for _, name := range []string{"op-slipstream-cycle-158960654", "op-velodrome-cycle-158954686"} {
		t.Run(name, func(t *testing.T) {
			base := "../../testdata/golden/" + name + "/"
			raw, err := os.ReadFile(base + "snapshot.json")
			if err != nil {
				t.Fatal(err)
			}
			store := state.New()
			if err = store.LoadSnapshot(raw); err != nil {
				t.Fatal(err)
			}
			var meta []PoolMetadata
			for _, p := range store.Pools() {
				meta = append(meta, PoolMetadata{Key: p.Key, Token0: p.Token0, Token1: p.Token1})
			}
			raw, err = os.ReadFile(base + "block.json")
			if err != nil {
				t.Fatal(err)
			}
			var record Record
			if err = json.Unmarshal(raw, &record); err != nil {
				t.Fatal(err)
			}
			raw, err = os.ReadFile(base + "expected.json")
			if err != nil {
				t.Fatal(err)
			}
			var expected struct {
				Index uint64      `json:"transaction_index"`
				Input string      `json:"input"`
				Gross string      `json:"actual_gross"`
				Cycle graph.Cycle `json:"cycle"`
			}
			if err = json.Unmarshal(raw, &expected); err != nil {
				t.Fatal(err)
			}
			tx := record.Transactions[expected.Index]
			got, reason := ExtractClosedCycle(record.Block, tx, meta, SwapDecoder{Matches: dex.IsSwapTopic, Amounts: dex.DecodeSwapAmounts})
			if reason != "" || got.Input.String() != expected.Input || got.ActualGross.String() != expected.Gross || !graph.SameDirectedCycle(got.Cycle, expected.Cycle, func(key domain.PoolKey) (domain.Address, domain.Address, bool) {
				p, ok := store.Pool(key)
				return p.Token0, p.Token1, ok
			}) {
				t.Fatalf("cycle=%+v reason=%s", got, reason)
			}
			tx.Logs = append(tx.Logs, tx.Logs...)
			if _, reason = ExtractClosedCycle(record.Block, tx, meta, SwapDecoder{Matches: dex.IsSwapTopic, Amounts: dex.DecodeSwapAmounts}); reason != "repeated_pool" {
				t.Fatal(reason)
			}
		})
	}
}

func TestReceiptDeficitReasonIsDeterministic(t *testing.T) {
	metadata := []PoolMetadata{
		{Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindV2}, Token0: domain.Address{10}, Token1: domain.Address{11}},
		{Key: domain.PoolKey{Address: domain.Address{2}, Kind: domain.PoolKindV2}, Token0: domain.Address{11}, Token1: domain.Address{12}},
		{Key: domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}, Token0: domain.Address{12}, Token1: domain.Address{10}},
	}
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte("Swap(address,uint256,uint256,uint256,uint256,address)"))
	var topic domain.Hash
	copy(topic[:], h.Sum(nil))
	tx := ingest.Transaction{Successful: true}
	for i, m := range metadata {
		data := make([]byte, 128)
		data[31] = 10
		data[127] = 11
		if i == 2 {
			data[127] = 9
		}
		tx.Logs = append(tx.Logs, ingest.Log{Address: m.Key.Address, Topics: []domain.Hash{topic}, Data: data})
	}
	// Two positive balances do not hide a third currency's deficit.
	for range 100 {
		if _, reason := ExtractClosedCycle(domain.BlockRef{}, tx, metadata, SwapDecoder{Matches: dex.IsSwapTopic, Amounts: dex.DecodeSwapAmounts}); reason != "flow_deficit" {
			t.Fatal(reason)
		}
	}
}
