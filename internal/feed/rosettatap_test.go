package feed

import (
	"os"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A Fast Feed frame from a local Nitro node tap, which carries the L1 gas
// split and logs of an arbitrary Nitro chain, decodes as a Fast Feed message.
func TestRosettaTapFrameDecodes(t *testing.T) {
	raw, err := os.ReadFile("testdata/rosettatap-frame.json")
	if err != nil {
		t.Fatal(err)
	}
	message, err := DecodeFastMessage(raw)
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := domain.ParseAddress("0x52e65b17fb6e5ba00ed806f37afcd2daa50271ca")
	if message.BlockNumber != 107_000_000 || message.TxIndex != 3 || !message.Receipt.Successful || message.Receipt.GasUsedForL1 != 5_000 ||
		len(message.Receipt.Logs) != 1 || message.Receipt.Logs[0].Address != pool || message.Receipt.EffectiveGasPrice.Int64() != 20_000_000 {
		t.Fatalf("decoded %+v", message)
	}
}
