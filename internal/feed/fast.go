package feed

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
)

// FastMessage carries one completed transaction, but no final block hash or
// system context. It can update a tentative pool view, never canonical state.
type FastMessage struct {
	TimestampMS uint64
	PGARound    uint64
	BlockNumber uint64
	TxIndex     uint64
	RawTx       []byte
	TxHash      domain.Hash
	Receipt     FastReceipt
}

type FastReceipt struct {
	Successful        bool
	GasUsed           uint64
	GasUsedForL1      uint64
	CumulativeGasUsed uint64
	EffectiveGasPrice *big.Int
	BaseFee           *big.Int
	ContractAddress   domain.Address
	Logs              []FastLog
}

type FastLog struct {
	Address domain.Address
	Topics  []domain.Hash
	Data    []byte
}

type fastWire struct {
	Version     uint64  `json:"version"`
	TimestampMS *uint64 `json:"timestamp_ms"`
	PGARound    uint64  `json:"pga_round"`
	Transaction *struct {
		BlockNumber *uint64 `json:"block_number"`
		TxIndex     *uint64 `json:"tx_index"`
		RawTx       string  `json:"raw_tx"`
		TxHash      string  `json:"tx_hash"`
		Receipt     *struct {
			Status            *fastQuantity `json:"status"`
			GasUsed           *fastQuantity `json:"gas_used"`
			GasUsedForL1      *fastQuantity `json:"gas_used_for_l1"`
			CumulativeGasUsed *fastQuantity `json:"cumulative_gas_used"`
			EffectiveGasPrice *fastQuantity `json:"effective_gas_price"`
			BaseFee           *fastQuantity `json:"base_fee"`
			ContractAddress   *string       `json:"contract_address"`
			Logs              *[]struct {
				Address string    `json:"address"`
				Topics  *[]string `json:"topics"`
				Data    string    `json:"data"`
			} `json:"logs"`
		} `json:"receipt"`
	} `json:"transaction"`
}

// DecodeFastMessage decodes the v1 binary-frame JSON schema. Receipt quantities
// accept unsigned JSON integers or hexadecimal strings. Raw transaction bytes
// must hash to tx_hash; RLP and chain validation belong to the EVM replay stage.
func DecodeFastMessage(data []byte) (FastMessage, error) {
	var wire fastWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return FastMessage{}, fmt.Errorf("fast feed: decode message: %w", err)
	}
	if wire.Version != 1 || wire.TimestampMS == nil || wire.Transaction == nil {
		return FastMessage{}, errors.New("fast feed: unsupported version or missing envelope fields")
	}
	tx := wire.Transaction
	if tx.BlockNumber == nil || tx.TxIndex == nil || tx.Receipt == nil || tx.Receipt.Status == nil || tx.Receipt.Logs == nil {
		return FastMessage{}, errors.New("fast feed: missing transaction, status or logs fields")
	}
	r := tx.Receipt
	if r.GasUsed == nil || r.GasUsedForL1 == nil || r.CumulativeGasUsed == nil || r.EffectiveGasPrice == nil || r.BaseFee == nil {
		return FastMessage{}, errors.New("fast feed: missing gas or fee quantities")
	}
	if !r.Status.IsUint64() || r.Status.Uint64() > 1 || !r.GasUsed.IsUint64() || !r.GasUsedForL1.IsUint64() || !r.CumulativeGasUsed.IsUint64() {
		return FastMessage{}, errors.New("fast feed: receipt status or gas quantity out of range")
	}
	raw, err := fastHex(tx.RawTx)
	if err != nil || len(raw) == 0 {
		return FastMessage{}, errors.New("fast feed: invalid raw_tx")
	}
	hash, err := domain.ParseHash(tx.TxHash)
	if err != nil {
		return FastMessage{}, fmt.Errorf("fast feed: tx_hash: %w", err)
	}
	digest := sha3.NewLegacyKeccak256()
	digest.Write(raw)
	if !bytes.Equal(digest.Sum(nil), hash[:]) {
		return FastMessage{}, errors.New("fast feed: raw_tx does not match tx_hash")
	}
	message := FastMessage{
		TimestampMS: *wire.TimestampMS, PGARound: wire.PGARound,
		BlockNumber: *tx.BlockNumber, TxIndex: *tx.TxIndex, RawTx: raw, TxHash: hash,
		Receipt: FastReceipt{
			Successful: r.Status.Uint64() == 1, GasUsed: r.GasUsed.Uint64(),
			GasUsedForL1: r.GasUsedForL1.Uint64(), CumulativeGasUsed: r.CumulativeGasUsed.Uint64(),
			EffectiveGasPrice: new(big.Int).Set(&r.EffectiveGasPrice.Int), BaseFee: new(big.Int).Set(&r.BaseFee.Int),
			Logs: make([]FastLog, len(*r.Logs)),
		},
	}
	if r.ContractAddress != nil {
		message.Receipt.ContractAddress, err = domain.ParseAddress(*r.ContractAddress)
		if err != nil {
			return FastMessage{}, fmt.Errorf("fast feed: contract_address: %w", err)
		}
	}
	if !message.Receipt.Successful && len(message.Receipt.Logs) != 0 {
		return FastMessage{}, errors.New("fast feed: failed receipt contains logs")
	}
	for i, log := range *r.Logs {
		decoded := &message.Receipt.Logs[i]
		decoded.Address, err = domain.ParseAddress(log.Address)
		if err != nil || log.Topics == nil || len(*log.Topics) > 4 {
			return FastMessage{}, fmt.Errorf("fast feed: log %d: invalid address or topics", i)
		}
		decoded.Data, err = fastHex(log.Data)
		if err != nil {
			return FastMessage{}, fmt.Errorf("fast feed: log %d: invalid data", i)
		}
		decoded.Topics = make([]domain.Hash, len(*log.Topics))
		for j, topic := range *log.Topics {
			decoded.Topics[j], err = domain.ParseHash(topic)
			if err != nil {
				return FastMessage{}, fmt.Errorf("fast feed: log %d topic %d: %w", i, j, err)
			}
		}
	}
	return message, nil
}

func fastHex(value string) ([]byte, error) {
	if !strings.HasPrefix(value, "0x") {
		return nil, errors.New("missing hex prefix")
	}
	return hex.DecodeString(value[2:])
}

// fastQuantity keeps fee values exact, including values above JSON's common
// floating-point integer limit. Negative, fractional and >256-bit values fail.
type fastQuantity struct{ big.Int }

func (q *fastQuantity) UnmarshalJSON(data []byte) error {
	value := string(data)
	base := 10
	if len(data) > 0 && data[0] == '"' {
		if err := json.Unmarshal(data, &value); err != nil {
			return err
		}
		if !strings.HasPrefix(value, "0x") {
			return errors.New("fast feed: quantity string requires hex prefix")
		}
		value, base = value[2:], 16
	}
	if value == "" || strings.ContainsAny(value, "+-.") {
		return errors.New("fast feed: invalid unsigned quantity")
	}
	if _, ok := q.SetString(value, base); !ok || q.Sign() < 0 || q.BitLen() > 256 {
		return errors.New("fast feed: invalid unsigned quantity")
	}
	return nil
}
