package remote

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestReplayDiffCompositionPreservesAndClearsSlots(t *testing.T) {
	address := addressAt(1).Hex()
	slot1, slot2 := wordHex(1), wordHex(2)
	base := map[string]any{address: map[string]any{"stateDiff": map[string]string{slot1: wordHex(10), slot2: wordHex(20)}, "balance": "0x20"}}
	balance := "0x30"
	nonce := uint64(2)
	diff := stateDiff{Pre: map[string]traceAccount{address: {Storage: map[string]string{slot1: wordHex(10)}}}, Post: map[string]traceAccount{address: {Balance: &balance, Nonce: &nonce}}}
	patch, err := diff.overrides()
	if err != nil {
		t.Fatal(err)
	}
	merged := mergeOverrides(base, patch)
	account := merged[address].(map[string]any)
	storage := account["stateDiff"].(map[string]string)
	if storage[slot1] != (domain.Hash{}).Hex() || storage[slot2] != wordHex(20) || account["balance"] != "0x30" || account["nonce"] != "0x2" {
		t.Fatal(merged)
	}
	if base[address].(map[string]any)["stateDiff"].(map[string]string)[slot1] != wordHex(10) {
		t.Fatal("composition mutated leased base")
	}
	deleted := stateDiff{Pre: map[string]traceAccount{address: {}}, Post: map[string]traceAccount{}}
	patch, err = deleted.overrides()
	if err != nil {
		t.Fatal(err)
	}
	merged = mergeOverrides(merged, patch)
	account = merged[address].(map[string]any)
	if account["code"] != "0x" || len(account["state"].(map[string]string)) != 0 || account["stateDiff"] != nil {
		t.Fatal("deleted state leaked", account)
	}
	next := mergeOverrides(merged, map[string]any{address: map[string]any{"stateDiff": map[string]string{slot2: wordHex(99)}}})
	if next[address].(map[string]any)["state"].(map[string]string)[slot2] != wordHex(99) {
		t.Fatal("post-delete storage write lost")
	}
}
