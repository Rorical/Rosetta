package remote

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"

	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
)

type flashPrefix struct {
	Base            feed.FlashblockBase
	Transactions    []feed.FlashTransaction
	Payload         string
	Index           uint64
	GasUsed         *uint64
	WithdrawalsRoot *domain.Hash
	BlobGasUsed     *uint64
	Version         domain.StateVersion
	Source          domain.Hash
	bytes           int
	seen            map[domain.Hash]bool
}

func (p *flashPrefix) append(block feed.Flashblock, encodedSize int) error {
	if block.MissingReceipts {
		return fmt.Errorf("Flashblocks prefix contains unhydrated receipts")
	}
	if block.Base != nil {
		p.Base = *block.Base
		p.Payload = block.PayloadID
	}
	if len(p.Transactions)+len(block.Transactions) > 4096 {
		return fmt.Errorf("Flashblocks prefix exceeds transaction bound")
	}
	p.bytes += encodedSize
	if p.bytes > 32<<20 {
		return fmt.Errorf("Flashblocks prefix exceeds byte bound")
	}
	for _, tx := range block.Transactions {
		if p.seen == nil {
			p.seen = make(map[domain.Hash]bool)
		}
		if p.seen[tx.Hash] {
			return fmt.Errorf("Flashblocks prefix repeats a transaction")
		}
		p.seen[tx.Hash] = true
		p.Transactions = append(p.Transactions, tx)
	}
	p.Index, p.GasUsed, p.WithdrawalsRoot, p.BlobGasUsed = block.Index, block.GasUsed, block.WithdrawalsRoot, block.BlobGasUsed
	return nil
}
func (p flashPrefix) freeze() flashPrefix { p.Transactions = slices.Clone(p.Transactions); return p }

// rawBlock uses a synthetic header identity. Native debug_traceBlock recovers
// signed OP/deposit transactions and applies the client's pre-execution hooks.
// No placeholder state root is presented as a verified chain root.
func (p flashPrefix) rawBlock() ([]byte, *flashHeader, error) {
	e := p.Base.Execution
	if e == nil || e.BaseFee == nil || e.BaseFee.Sign() <= 0 || p.GasUsed == nil || p.WithdrawalsRoot == nil || len(p.Transactions) == 0 || p.Base.Number != p.Version.Anchor.Number+1 || p.Base.Parent != p.Version.Anchor.Hash || p.Base.Timestamp <= p.Version.Anchor.Timestamp || *p.GasUsed > e.GasLimit {
		return nil, nil, fmt.Errorf("Flashblocks prefix lacks complete execution context")
	}
	header := &flashHeader{
		ParentHash: p.Base.Parent, UncleHash: digest([]byte{0xc0}), Coinbase: e.Coinbase,
		Root: digest([]byte{0x80}), TxHash: digest([]byte{0x80}), ReceiptHash: digest([]byte{0x80}),
		Difficulty: new(big.Int), Number: p.Base.Number,
		GasLimit: e.GasLimit, GasUsed: *p.GasUsed, Time: p.Base.Timestamp,
		Extra: bytes.Clone(e.Extra), MixDigest: e.Random, BaseFee: e.BaseFee,
		WithdrawalsHash: *p.WithdrawalsRoot, ParentBeaconRoot: e.BeaconRoot,
	}
	if p.BlobGasUsed != nil {
		header.BlobGasUsed = *p.BlobGasUsed
	}
	// Each typed transaction is an RLP byte string; legacy transactions already
	// encode an RLP list and must not be wrapped a second time.
	txs := make([]rlp.RawValue, len(p.Transactions))
	for i, tx := range p.Transactions {
		if len(tx.Raw) == 0 || digest(tx.Raw) != tx.Hash {
			return nil, nil, fmt.Errorf("Flashblocks raw transaction identity mismatch")
		}
		if tx.Raw[0] >= 0xc0 {
			txs[i] = tx.Raw
		} else {
			encoded, err := rlp.EncodeToBytes(tx.Raw)
			if err != nil {
				return nil, nil, err
			}
			txs[i] = encoded
		}
	}
	raw, err := rlp.EncodeToBytes([]any{header, txs, []any{}, []any{}})
	return raw, header, err
}
func (p flashPrefix) contextHash() domain.Hash {
	encoded, _ := json.Marshal(struct {
		Base            feed.FlashblockBase
		Payload         string
		Index           uint64
		Version         domain.StateVersion
		GasUsed         *uint64
		WithdrawalsRoot *domain.Hash
		BlobGasUsed     *uint64
	}{p.Base, p.Payload, p.Index, p.Version, p.GasUsed, p.WithdrawalsRoot, p.BlobGasUsed})
	return digest(encoded)
}
func flashSource(rpcURL, feedURL string) domain.Hash {
	return digest([]byte("op-rpc-flashblocks-v1\x00" + rpcURL + "\x00" + feedURL))
}
func flashHex(raw []byte) string { return "0x" + hex.EncodeToString(raw) }

// flashHeader is the post-Isthmus execution header RLP layout. Keeping opaque
// transaction envelopes avoids linking the RPC build to an embedded EVM.
type flashHeader struct {
	ParentHash, UncleHash           domain.Hash
	Coinbase                        domain.Address
	Root, TxHash, ReceiptHash       domain.Hash
	Bloom                           [256]byte
	Difficulty                      *big.Int
	Number, GasLimit, GasUsed, Time uint64
	Extra                           []byte
	MixDigest                       domain.Hash
	Nonce                           [8]byte
	BaseFee                         *big.Int
	WithdrawalsHash                 domain.Hash
	BlobGasUsed, ExcessBlobGas      uint64
	ParentBeaconRoot                domain.Hash
}
