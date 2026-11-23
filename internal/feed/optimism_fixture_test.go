package feed

import (
	"bytes"
	"os"
	"testing"

	"github.com/andybalholm/brotli"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Public OP Mainnet Subblocks capture, block 158938803/index 9.
// Source: wss://op-mainnet-fb-ws-pub.optimism.io/ws.
func TestOptimismSubblocksCompleteReceiptsFixture(t *testing.T) {
	raw, err := os.ReadFile("testdata/optimism-subblocks-receipts.json")
	if err != nil {
		t.Fatal(err)
	}
	var compressed bytes.Buffer
	writer := brotli.NewWriter(&compressed)
	if _, err := writer.Write(raw); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	for _, frame := range [][]byte{raw, compressed.Bytes()} {
		block, err := DecodeFlashblock(frame)
		if err != nil {
			t.Fatal(err)
		}
		if block.Number != 158938803 || block.Index != 9 || block.MissingReceipts || len(block.Transactions) != 3 {
			t.Fatalf("unexpected captured frame: %+v", block)
		}
		count := 0
		for _, tx := range block.Transactions {
			count += len(tx.Logs)
		}
		if count != 16 {
			t.Fatalf("logs = %d, want 16", count)
		}
		want, err := domain.ParseHash("0xe1893f046e0cff80b9e3bb366d87512907698699894cde6079d850e333274dfa")
		if err != nil {
			t.Fatal(err)
		}
		if block.Transactions[0].Hash != want {
			t.Fatal("transaction/receipt identity changed")
		}
	}
}
