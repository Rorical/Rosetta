package feed

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestFlashblocksExecutionContext(t *testing.T) {
	frame := flashFixture()
	base := frame["base"].(map[string]any)
	base["fee_recipient"] = "0x4200000000000000000000000000000000000011"
	base["prev_randao"] = "0x" + strings.Repeat("11", 32)
	base["parent_beacon_block_root"] = "0x" + strings.Repeat("22", 32)
	base["gas_limit"], base["base_fee_per_gas"], base["extra_data"] = "0x1c9c380", "0x100", "0x010203"
	diff := frame["diff"].(map[string]any)
	diff["gas_used"] = "0x1000"
	diff["withdrawals_root"] = "0x" + strings.Repeat("33", 32)
	diff["withdrawals"] = []any{}
	raw, _ := json.Marshal(frame)
	block, err := DecodeFlashblock(raw)
	if err != nil || block.Base.Execution == nil || block.Base.Execution.BaseFee.Uint64() != 256 || block.GasUsed == nil || *block.GasUsed != 4096 || block.WithdrawalsRoot == nil {
		t.Fatal(block, err)
	}
	delete(base, "prev_randao")
	raw, _ = json.Marshal(frame)
	if _, err := DecodeFlashblock(raw); err == nil {
		t.Fatal("partial header context accepted")
	}
}

func TestFlashblocksEnvelopeRequiresExplicitReceiptCompletion(t *testing.T) {
	frame := flashFixture()
	delete(frame["metadata"].(map[string]any), "receipts")
	raw, _ := json.Marshal(frame)
	if _, err := DecodeFlashblock(raw); err == nil {
		t.Fatal("strict receipt decoder accepted stripped effects")
	}
	block, err := DecodeFlashblockEnvelope(raw)
	if err != nil || !block.MissingReceipts || len(block.Transactions) != 1 || block.Transactions[0].Successful || len(block.Transactions[0].Logs) != 0 {
		t.Fatal(block, err)
	}
}

// Captured from Base's raw infrastructure stream. Receipt omission is a real
// transport variant; the envelope remains unfit for pool updates until hydrated.
func TestBaseIndexZeroExecutionContextFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/base-flashblocks-index-zero.json")
	if err != nil {
		t.Fatal(err)
	}
	block, err := DecodeFlashblockEnvelope(raw)
	if err != nil || block.Index != 0 || !block.MissingReceipts || block.Base == nil || block.Base.Execution == nil || block.GasUsed == nil || block.WithdrawalsRoot == nil || block.BlobGasUsed == nil || len(block.Transactions) == 0 {
		t.Fatal(block, err)
	}
	if block.Base.Execution.BaseFee.Sign() <= 0 || block.Base.Execution.GasLimit == 0 || block.Base.Timestamp == 0 {
		t.Fatal("incomplete observed Base context")
	}
	if _, err := DecodeFlashblock(raw); err == nil {
		t.Fatal("captured stripped effects treated as complete receipts")
	}
}
