package remote

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

var testCoinbase = domain.Address{0x42, 0x11}

// testBlock is one block of a test chain; the RPC and the stream serve the
// same content unless a test changes one of them.
type testBlock struct {
	ref          domain.BlockRef
	transactions []ingest.Transaction
}

func testBlocks(parent domain.BlockRef, count int, pools []pool.State) []testBlock {
	var blocks []testBlock
	for i := range count {
		ref := domain.BlockRef{Number: parent.Number + 1, Hash: domain.Hash{0xb0, byte(parent.Number + 1)}, ParentHash: parent.Hash, Timestamp: 1_000 + parent.Number + 1}
		sync := syncEvent(pools[i%len(pools)].Key, 2_000_000+int64(ref.Number))
		blocks = append(blocks, testBlock{ref: ref, transactions: []ingest.Transaction{
			{Hash: domain.Hash{0xd0, byte(ref.Number)}, Index: 0, Successful: true},
			{Hash: domain.Hash{0xe0, byte(ref.Number)}, Index: 1, Successful: true, Logs: []ingest.Log{{Address: sync.Address, Topics: sync.Topics, Data: sync.Data}}},
			{Hash: domain.Hash{0xf0, byte(ref.Number)}, Index: 2},
		}})
		parent = ref
	}
	return blocks
}

func (b testBlock) header() map[string]any {
	hashes := []string{}
	for _, tx := range b.transactions {
		hashes = append(hashes, tx.Hash.Hex())
	}
	return map[string]any{"number": fmt.Sprintf("0x%x", b.ref.Number), "hash": b.ref.Hash.Hex(), "parentHash": b.ref.ParentHash.Hex(), "timestamp": fmt.Sprintf("0x%x", b.ref.Timestamp), "baseFeePerGas": "0x64", "gasLimit": "0x1c9c380", "miner": testCoinbase.Hex(), "transactions": hashes}
}

func (b testBlock) receipts() []any {
	var out []any
	logIndex := 0
	for i, tx := range b.transactions {
		logs := []any{}
		for _, log := range tx.Logs {
			topics := []string{}
			for _, topic := range log.Topics {
				topics = append(topics, topic.Hex())
			}
			logs = append(logs, map[string]any{"address": log.Address.Hex(), "topics": topics, "data": "0x" + hex.EncodeToString(log.Data), "logIndex": fmt.Sprintf("0x%x", logIndex), "blockNumber": fmt.Sprintf("0x%x", b.ref.Number), "blockHash": b.ref.Hash.Hex(), "transactionHash": tx.Hash.Hex()})
			logIndex++
		}
		status := "0x0"
		if tx.Successful {
			status = "0x1"
		}
		out = append(out, map[string]any{"transactionHash": tx.Hash.Hex(), "transactionIndex": fmt.Sprintf("0x%x", i), "blockNumber": fmt.Sprintf("0x%x", b.ref.Number), "blockHash": b.ref.Hash.Hex(), "status": status, "logs": logs,
			"type": "0x2", "gasUsed": "0x5208", "effectiveGasPrice": "0x70", "l1Fee": "0x5", "l1GasPrice": "0x7", "l1BlobBaseFee": "0x1", "l1BaseFeeScalar": "0x2", "l1BlobBaseFeeScalar": "0x3"})
	}
	return out
}

// frames splits a block into two frames of one payload.
func (b testBlock) frames(payload string) []feed.Flashblock {
	var txs []feed.FlashTransaction
	for _, tx := range b.transactions {
		ftx := feed.FlashTransaction{Hash: tx.Hash, Successful: tx.Successful}
		for _, log := range tx.Logs {
			ftx.Logs = append(ftx.Logs, feed.FastLog{Address: log.Address, Topics: log.Topics, Data: log.Data})
		}
		txs = append(txs, ftx)
	}
	base := &feed.FlashblockBase{Parent: b.ref.ParentHash, Number: b.ref.Number, Timestamp: b.ref.Timestamp, Execution: &feed.FlashExecution{Coinbase: testCoinbase, GasLimit: 30_000_000, BaseFee: big.NewInt(100)}}
	return []feed.Flashblock{
		{PayloadID: payload, Number: b.ref.Number, Index: 0, Base: base, Transactions: txs[:1]},
		{PayloadID: payload, Number: b.ref.Number, Index: 1, Transactions: txs[1:]},
	}
}

func assemble(frames ...feed.Flashblock) []flashSeal {
	out := make(chan flashSeal, 64)
	a := flashAssembler{out: out}
	for _, frame := range frames {
		a.frame(frame, false)
	}
	close(out)
	var seals []flashSeal
	for seal := range out {
		seals = append(seals, seal)
	}
	return seals
}

// streamOf seals blocks as the stream would: each block is sealed by the
// index-zero frame of the next one.
func streamOf(blocks []testBlock, next testBlock) []flashSeal {
	var frames []feed.Flashblock
	for i, block := range append(blocks, next) {
		frames = append(frames, block.frames(fmt.Sprintf("0x%016x", i+1))...)
	}
	var seals []flashSeal
	for _, seal := range assemble(frames...) {
		if seal.block != nil {
			seals = append(seals, seal)
		}
	}
	return seals
}

func TestAssemblerSealsBlockWithTheNextBaseParent(t *testing.T) {
	store, pools := poolFixture(t)
	blocks := testBlocks(store.Block(), 2, pools)
	frames := append(blocks[0].frames("0x0000000000000001"), blocks[1].frames("0x0000000000000002")[0])
	seals := assemble(frames...)
	if len(seals) != 2 || seals[0].number != 10 || seals[0].hash != store.Block().Hash || seals[0].block != nil {
		t.Fatal("first index zero must only hint the parent it names", seals)
	}
	sealed := seals[1]
	if sealed.number != 11 || sealed.hash != blocks[0].ref.Hash || sealed.block == nil {
		t.Fatal("block 11 not sealed by the next base parent", sealed)
	}
	got := sealed.block.block
	if got.Ref != blocks[0].ref || got.GasLimit != 30_000_000 || got.Coinbase != testCoinbase || got.BaseFee.Int64() != 100 {
		t.Fatal("stream header", got)
	}
	if !sameTransactionsExceptFees(sealed.block.transactions, blocks[0].transactions) || len(got.Transactions) != 3 || got.Transactions[2] != blocks[0].transactions[2].Hash {
		t.Fatal("stream transactions differ across frames", sealed.block.transactions)
	}
}

func TestAssemblerTrustsOnlyWholeStreamBlocks(t *testing.T) {
	store, pools := poolFixture(t)
	blocks := testBlocks(store.Block(), 3, pools)
	whole := func(frames ...feed.Flashblock) map[uint64]bool {
		out := make(chan flashSeal, 64)
		a := flashAssembler{out: out}
		for _, frame := range frames {
			a.frame(frame, frame.MissingReceipts)
			if frame.PayloadID == "reset" {
				a.reset()
			}
		}
		close(out)
		trusted := map[uint64]bool{}
		for seal := range out {
			trusted[seal.number] = seal.block != nil
		}
		return trusted
	}
	first, second, third := blocks[0].frames("0x0000000000000001"), blocks[1].frames("0x0000000000000002"), blocks[2].frames("0x0000000000000003")
	t.Run("payload replaced", func(t *testing.T) {
		replacement := blocks[0].frames("0x0000000000000009")
		if got := whole(first[0], replacement[0], replacement[1], second[0]); got[11] {
			t.Fatal("replaced payload trusted")
		}
	})
	t.Run("receipts completed by RPC", func(t *testing.T) {
		hydrated := first[1]
		hydrated.MissingReceipts = true
		if got := whole(first[0], hydrated, second[0]); got[11] {
			t.Fatal("hydrated block trusted")
		}
	})
	t.Run("stream reset", func(t *testing.T) {
		cut := first[0]
		cut.PayloadID = "reset"
		if got := whole(cut, second[0], second[1], third[0]); got[11] || !got[12] {
			t.Fatal("block cut by a reset trusted, or the next whole block not", got)
		}
	})
	t.Run("gap", func(t *testing.T) {
		if got := whole(first[0], first[1], third[0]); got[11] || got[12] {
			t.Fatal("block before a missing height trusted", got)
		}
	})
	t.Run("no execution context", func(t *testing.T) {
		bare := first[0]
		base := *bare.Base
		base.Execution = nil
		bare.Base = &base
		if got := whole(bare, first[1], second[0]); got[11] {
			t.Fatal("block without base fee trusted")
		}
	})
}

func l1InfoTransaction(t *testing.T, sequence uint64, basefee int64) []byte {
	t.Helper()
	data := make([]byte, 178)
	copy(data, []byte{0x3d, 0xb6, 0xbe, 0x2b})
	data[7], data[11] = 7, 9
	data[19] = byte(sequence)
	big.NewInt(basefee).FillBytes(data[36:68])
	data[100] = byte(sequence) // L1 hash moves with the epoch, not the fees
	data[167] = 5
	encoded, err := rlp.EncodeToBytes([]any{make([]byte, 32), make([]byte, 20), opL1Block[:], uint64(0), uint64(0), uint64(1_000_000), false, data})
	if err != nil {
		t.Fatal(err)
	}
	return append([]byte{opDepositType}, encoded...)
}

func TestL1InfoFeeParamsKeyIgnoresL1BaseFees(t *testing.T) {
	first := l1InfoFeeParams(l1InfoTransaction(t, 1, 1000))
	if first.key == "" || first.key != l1InfoFeeParams(l1InfoTransaction(t, 2, 1000)).key {
		t.Fatal("key moved with the sequence number or L1 hash", first.key)
	}
	moved := l1InfoFeeParams(l1InfoTransaction(t, 1, 1001))
	if moved.key != first.key || first.l1Rate.Int64() != 16*7*1000 || moved.l1Rate.Int64() != 16*7*1001 {
		t.Fatal("an L1 base fee change must move only the rate", first, moved)
	}
	scalar := l1InfoTransaction(t, 1, 1000)
	scalar[len(scalar)-178+7] = 8
	if l1InfoFeeParams(scalar).key == first.key {
		t.Fatal("key ignored the base fee scalar")
	}
	user := l1InfoTransaction(t, 1, 1000)
	user[0] = 0x02
	if l1InfoFeeParams(user).key != "" || l1InfoFeeParams(nil).key != "" {
		t.Fatal("non-deposit transaction fingerprinted")
	}
}

// OP Mainnet blocks 159059589 and 159059789: the gas price
// oracle's getL1FeeUpperBound for 1700 and 400 bytes at both blocks, and the
// L1 rate from the L1Block scalars and base fees. Rescaling the first block's
// samples must not fall below the oracle and may exceed it by a few wei.
func TestReactiveFeesRescaleToTheOracleAtAnotherL1Rate(t *testing.T) {
	read := &reactiveFees{l1: big.NewInt(36525480251), l1Low: big.NewInt(9117047348), operator: big.NewInt(7), operatorLow: big.NewInt(3)}
	from, to := big.NewInt(25107747252897), big.NewInt(28049751234857)
	scaled := read.at(from, to)
	for _, c := range []struct{ got, oracle int64 }{{scaled.l1.Int64(), 40805358779}, {scaled.l1Low.Int64(), 10185338714}} {
		if c.got < c.oracle || c.got > c.oracle+2 {
			t.Fatalf("rescaled %d, oracle %d", c.got, c.oracle)
		}
	}
	if scaled.operator != read.operator || scaled.operatorLow != read.operatorLow || read.at(from, from) != read || read.at(nil, to) != read {
		t.Fatal("operator fees and unknown or equal rates must keep the read")
	}
}
