package feed

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Flashblock carries one sequencer transaction batch. MissingReceipts requires
// completion before pool updates. Placeholder state/block roots are not exposed.
type Flashblock struct {
	PayloadID       string
	Index, Number   uint64
	Base            *FlashblockBase
	Transactions    []FlashTransaction
	GasUsed         *uint64
	WithdrawalsRoot *domain.Hash
	BlobGasUsed     *uint64
	MissingReceipts bool
}

type FlashblockBase struct {
	Parent            domain.Hash
	Number, Timestamp uint64
	Execution         *FlashExecution
}

type FlashTransaction struct {
	Hash       domain.Hash
	Raw        []byte
	Successful bool
	Logs       []FastLog
}

// DecodeFlashblock accepts JSON and the Brotli-compressed JSON used by
// Unichain. It requires receipts for every transaction; a hash or bloom cannot
// substitute for execution logs. A stripped-metadata stream is rejected.
func DecodeFlashblock(frame []byte) (Flashblock, error) { return decodeFlashblock(frame, true) }

// DecodeFlashblockEnvelope retains signed transactions when a raw stream omits
// receipts. Callers must hydrate every receipt before using any pool effects.
func DecodeFlashblockEnvelope(frame []byte) (Flashblock, error) {
	return decodeFlashblock(frame, false)
}

func decodeFlashblock(frame []byte, requireReceipts bool) (Flashblock, error) {
	if len(frame) == 0 || len(frame) > 32<<20 {
		return Flashblock{}, fmt.Errorf("flashblocks: invalid frame size")
	}
	if !json.Valid(frame) {
		decoded, err := io.ReadAll(io.LimitReader(brotli.NewReader(bytes.NewReader(frame)), (32<<20)+1))
		if err != nil || len(decoded) > 32<<20 {
			return Flashblock{}, fmt.Errorf("flashblocks: invalid compressed frame")
		}
		frame = decoded
	}
	var wire struct {
		PayloadID string  `json:"payload_id"`
		Index     *uint64 `json:"index"`
		Base      *struct {
			Parent    string `json:"parent_hash"`
			Number    string `json:"block_number"`
			Timestamp string `json:"timestamp"`
		} `json:"base"`
		Diff *struct {
			Transactions    *[]string         `json:"transactions"`
			Gas             *string           `json:"gas_used"`
			WithdrawalsRoot *string           `json:"withdrawals_root"`
			BlobGasUsed     *string           `json:"blob_gas_used"`
			Withdrawals     []json.RawMessage `json:"withdrawals"`
		} `json:"diff"`
		Metadata *struct {
			Number   *uint64                    `json:"block_number"`
			Receipts map[string]json.RawMessage `json:"receipts"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(frame, &wire); err != nil {
		return Flashblock{}, fmt.Errorf("flashblocks: decode: %w", err)
	}
	id, err := flashBytes(wire.PayloadID)
	if err != nil || len(id) != 8 || wire.Index == nil || wire.Diff == nil || wire.Diff.Transactions == nil || wire.Metadata == nil || wire.Metadata.Number == nil || requireReceipts && wire.Metadata.Receipts == nil {
		return Flashblock{}, fmt.Errorf("flashblocks: missing payload identity, transactions or receipts")
	}
	block := Flashblock{PayloadID: wire.PayloadID, Index: *wire.Index, Number: *wire.Metadata.Number, MissingReceipts: wire.Metadata.Receipts == nil}
	if block.Index == 0 && wire.Base == nil || block.Index != 0 && wire.Base != nil {
		return Flashblock{}, fmt.Errorf("flashblocks: base must appear exactly at index zero")
	}
	if wire.Base != nil {
		parent, err := domain.ParseHash(wire.Base.Parent)
		if err != nil || parent == (domain.Hash{}) {
			return Flashblock{}, fmt.Errorf("flashblocks: invalid parent")
		}
		number, err := flashQuantity(wire.Base.Number)
		if err != nil || number != block.Number {
			return Flashblock{}, fmt.Errorf("flashblocks: inconsistent block number")
		}
		timestamp, err := flashQuantity(wire.Base.Timestamp)
		if err != nil {
			return Flashblock{}, err
		}
		var envelope struct {
			Base json.RawMessage `json:"base"`
		}
		if err := json.Unmarshal(frame, &envelope); err != nil {
			return Flashblock{}, err
		}
		execution, err := decodeFlashExecution(envelope.Base)
		if err != nil {
			return Flashblock{}, err
		}
		block.Base = &FlashblockBase{Parent: parent, Number: number, Timestamp: timestamp, Execution: execution}
	}
	if wire.Diff.Gas != nil {
		gas, err := flashQuantity(*wire.Diff.Gas)
		if err != nil {
			return Flashblock{}, err
		}
		block.GasUsed = &gas
	}
	if wire.Diff.WithdrawalsRoot != nil {
		root, err := domain.ParseHash(*wire.Diff.WithdrawalsRoot)
		if err != nil {
			return Flashblock{}, err
		}
		block.WithdrawalsRoot = &root
	}
	if len(wire.Diff.Withdrawals) != 0 {
		return Flashblock{}, fmt.Errorf("flashblocks: nonempty withdrawals are unsupported")
	}
	if wire.Diff.BlobGasUsed != nil {
		gas, err := flashQuantity(*wire.Diff.BlobGasUsed)
		if err != nil {
			return Flashblock{}, err
		}
		block.BlobGasUsed = &gas
	}
	if len(*wire.Diff.Transactions) > 4096 || !block.MissingReceipts && len(wire.Metadata.Receipts) != len(*wire.Diff.Transactions) {
		return Flashblock{}, fmt.Errorf("flashblocks: receipt count differs from transaction batch")
	}
	seen := make(map[domain.Hash]bool)
	for _, encoded := range *wire.Diff.Transactions {
		raw, err := flashBytes(encoded)
		if err != nil || len(raw) == 0 {
			return Flashblock{}, fmt.Errorf("flashblocks: invalid raw transaction")
		}
		h := sha3.NewLegacyKeccak256()
		h.Write(raw)
		var hash domain.Hash
		copy(hash[:], h.Sum(nil))
		if seen[hash] {
			return Flashblock{}, fmt.Errorf("flashblocks: duplicate transaction")
		}
		seen[hash] = true
		if block.MissingReceipts {
			block.Transactions = append(block.Transactions, FlashTransaction{Hash: hash, Raw: raw})
			continue
		}
		receipt, exists := wire.Metadata.Receipts[hash.Hex()]
		if !exists {
			return Flashblock{}, fmt.Errorf("flashblocks: missing receipt for %s", hash)
		}
		success, logs, err := flashReceipt(receipt)
		if err != nil {
			return Flashblock{}, err
		}
		block.Transactions = append(block.Transactions, FlashTransaction{Hash: hash, Raw: raw, Successful: success, Logs: logs})
	}
	return block, nil
}

func flashReceipt(raw json.RawMessage) (bool, []FastLog, error) {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false, nil, fmt.Errorf("flashblocks: invalid receipt")
	}
	if _, exists := object["status"]; !exists {
		if len(object) != 1 {
			return false, nil, fmt.Errorf("flashblocks: invalid receipt envelope")
		}
		for _, value := range object {
			raw = value
		}
	}
	var receipt struct {
		Status *fastQuantity `json:"status"`
		Logs   *[]struct {
			Address string    `json:"address"`
			Topics  *[]string `json:"topics"`
			Data    *string   `json:"data"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(raw, &receipt); err != nil || receipt.Status == nil || receipt.Status.Sign() < 0 || !receipt.Status.IsUint64() || receipt.Status.Uint64() > 1 || receipt.Logs == nil {
		return false, nil, fmt.Errorf("flashblocks: missing or invalid receipt status/logs")
	}
	logs := make([]FastLog, 0, len(*receipt.Logs))
	for _, entry := range *receipt.Logs {
		address, err := domain.ParseAddress(entry.Address)
		if err != nil || entry.Topics == nil || len(*entry.Topics) > 4 || entry.Data == nil {
			return false, nil, fmt.Errorf("flashblocks: invalid log")
		}
		data, err := flashBytes(*entry.Data)
		if err != nil {
			return false, nil, err
		}
		log := FastLog{Address: address, Data: data}
		for _, topic := range *entry.Topics {
			hash, err := domain.ParseHash(topic)
			if err != nil {
				return false, nil, fmt.Errorf("flashblocks: invalid topic")
			}
			log.Topics = append(log.Topics, hash)
		}
		logs = append(logs, log)
	}
	if receipt.Status.Uint64() == 0 && len(logs) > 0 {
		return false, nil, fmt.Errorf("flashblocks: reverted transaction contains logs")
	}
	return receipt.Status.Uint64() == 1, logs, nil
}

func flashBytes(value string) ([]byte, error) {
	raw, ok := strings.CutPrefix(value, "0x")
	if !ok {
		return nil, fmt.Errorf("flashblocks: missing hex prefix")
	}
	return hex.DecodeString(raw)
}
func flashQuantity(value string) (uint64, error) {
	raw, ok := strings.CutPrefix(value, "0x")
	if !ok || raw == "" {
		return 0, fmt.Errorf("flashblocks: invalid quantity")
	}
	return strconv.ParseUint(raw, 16, 64)
}
