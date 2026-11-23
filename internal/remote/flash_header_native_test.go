package remote

import (
	"os"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
)

// Decode with the independent execution client's header type, rather than the
// replay encoder's own struct. The opaque OP deposit envelope stays untouched.
func TestFlashblocksHeaderLayoutAgainstExecutionClient(t *testing.T) {
	encoded, err := os.ReadFile("../feed/testdata/base-flashblocks-index-zero.json")
	if err != nil {
		t.Fatal(err)
	}
	block, err := feed.DecodeFlashblockEnvelope(encoded)
	if err != nil {
		t.Fatal(err)
	}
	parent := domain.BlockRef{Number: block.Number - 1, Hash: block.Base.Parent, Timestamp: block.Base.Timestamp - 2}
	prefix := flashPrefix{Base: *block.Base, Transactions: block.Transactions, GasUsed: block.GasUsed, BlobGasUsed: block.BlobGasUsed, WithdrawalsRoot: block.WithdrawalsRoot, Version: domain.StateVersion{Anchor: parent}}
	raw, _, err := prefix.rawBlock()
	if err != nil {
		t.Fatal(err)
	}
	var body []rlp.RawValue
	if err := rlp.DecodeBytes(raw, &body); err != nil || len(body) != 4 {
		t.Fatal(err)
	}
	var header types.Header
	if err := rlp.DecodeBytes(body[0], &header); err != nil {
		t.Fatal(err)
	}
	context := block.Base.Execution
	if header.Number.Uint64() != block.Number || header.ParentHash != common.Hash(parent.Hash) || header.Time != block.Base.Timestamp || header.GasLimit != context.GasLimit || header.GasUsed != *block.GasUsed || header.BaseFee.Cmp(context.BaseFee) != 0 || header.Coinbase != common.Address(context.Coinbase) || header.MixDigest != common.Hash(context.Random) || header.ParentBeaconRoot == nil || *header.ParentBeaconRoot != common.Hash(context.BeaconRoot) || header.WithdrawalsHash == nil || *header.WithdrawalsHash != common.Hash(*block.WithdrawalsRoot) || header.BlobGasUsed == nil || *header.BlobGasUsed != *block.BlobGasUsed {
		t.Fatal("native client decoded different Flashblocks header context")
	}
	var transactions [][]byte
	if err := rlp.DecodeBytes(body[1], &transactions); err != nil || len(transactions) != len(block.Transactions) {
		t.Fatal(err)
	}
	for i, raw := range transactions {
		if digest(raw) != block.Transactions[i].Hash {
			t.Fatal("raw OP envelope changed")
		}
	}
}
