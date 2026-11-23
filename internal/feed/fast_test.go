package feed

import (
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"

	"golang.org/x/crypto/sha3"
)

// Synthetic schema fixture; raw_tx is only hashed here, not EVM decoded.
func fastFrame() map[string]any {
	raw := []byte{2, 0xc0}
	hash := sha3.NewLegacyKeccak256()
	hash.Write(raw)
	return map[string]any{
		"version": 1, "timestamp_ms": uint64(1793400000123), "pga_round": 2,
		"transaction": map[string]any{
			"block_number": 11, "tx_index": 0, "raw_tx": "0x" + hex.EncodeToString(raw),
			"tx_hash": "0x" + hex.EncodeToString(hash.Sum(nil)),
			"receipt": map[string]any{
				"status": 1, "gas_used": "0x5208", "gas_used_for_l1": 100,
				"cumulative_gas_used": 22000, "effective_gas_price": "0x20000000000001", "base_fee": 10000000,
				"logs": []any{map[string]any{
					"address": "0x" + strings.Repeat("11", 20),
					"topics":  []string{"0x" + strings.Repeat("22", 32)}, "data": "0x1234",
				}},
			},
		},
	}
}

func encodeFastFrame(t *testing.T, frame map[string]any) []byte {
	t.Helper()
	data, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestDecodeFastMessage(t *testing.T) {
	message, err := DecodeFastMessage(encodeFastFrame(t, fastFrame()))
	if err != nil {
		t.Fatal(err)
	}
	if message.BlockNumber != 11 || message.TxIndex != 0 || message.PGARound != 2 || message.TimestampMS != 1793400000123 {
		t.Fatalf("lost position: %+v", message)
	}
	if !message.Receipt.Successful || message.Receipt.GasUsed != 21000 || message.Receipt.EffectiveGasPrice.String() != "9007199254740993" {
		t.Fatalf("lost exact receipt quantities: %+v", message.Receipt)
	}
	if len(message.Receipt.Logs) != 1 || hex.EncodeToString(message.Receipt.Logs[0].Data) != "1234" {
		t.Fatal("lost receipt log")
	}
	frame := fastFrame()
	receipt := frame["transaction"].(map[string]any)["receipt"].(map[string]any)
	receipt["status"], receipt["logs"] = 0, []any{}
	message, err = DecodeFastMessage(encodeFastFrame(t, frame))
	if err != nil || message.Receipt.Successful || len(message.Receipt.Logs) != 0 {
		t.Fatalf("failed transaction: %+v %v", message, err)
	}
}

func TestDecodeFastMessageRejectsIncompleteOrConflictingEffects(t *testing.T) {
	tests := []struct {
		name string
		edit func(map[string]any, map[string]any, map[string]any)
	}{
		{"unknown version", func(f, tx, r map[string]any) { f["version"] = 2 }},
		{"missing timestamp", func(f, tx, r map[string]any) { delete(f, "timestamp_ms") }},
		{"missing index", func(f, tx, r map[string]any) { delete(tx, "tx_index") }},
		{"missing block", func(f, tx, r map[string]any) { delete(tx, "block_number") }},
		{"hash mismatch", func(f, tx, r map[string]any) { tx["raw_tx"] = "0xc0" }},
		{"empty transaction", func(f, tx, r map[string]any) { tx["raw_tx"] = "0x" }},
		{"missing receipt", func(f, tx, r map[string]any) { delete(tx, "receipt") }},
		{"missing status", func(f, tx, r map[string]any) { delete(r, "status") }},
		{"missing fee", func(f, tx, r map[string]any) { delete(r, "base_fee") }},
		{"invalid status", func(f, tx, r map[string]any) { r["status"] = 2 }},
		{"missing logs", func(f, tx, r map[string]any) { delete(r, "logs") }},
		{"null logs", func(f, tx, r map[string]any) { r["logs"] = nil }},
		{"failed receipt effects", func(f, tx, r map[string]any) { r["status"] = 0 }},
		{"negative gas", func(f, tx, r map[string]any) { r["gas_used"] = -1 }},
		{"gas overflow", func(f, tx, r map[string]any) { r["gas_used"] = "0x10000000000000000" }},
		{"fractional fee", func(f, tx, r map[string]any) { r["base_fee"] = 1.5 }},
		{"fee overflow", func(f, tx, r map[string]any) { r["base_fee"] = "0x1" + strings.Repeat("0", 64) }},
		{"missing topic array", func(f, tx, r map[string]any) { delete(r["logs"].([]any)[0].(map[string]any), "topics") }},
		{"missing log data", func(f, tx, r map[string]any) { delete(r["logs"].([]any)[0].(map[string]any), "data") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			frame := fastFrame()
			tx := frame["transaction"].(map[string]any)
			test.edit(frame, tx, tx["receipt"].(map[string]any))
			if _, err := DecodeFastMessage(encodeFastFrame(t, frame)); err == nil {
				t.Fatal("accepted invalid frame")
			}
		})
	}
}
