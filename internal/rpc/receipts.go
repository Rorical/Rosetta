package rpc

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Receipt is the subset of a transaction receipt that log consumers need.
type Receipt struct {
	TransactionHash domain.Hash
	Logs            []Log
}

// BlockReceipts returns every receipt in the block identified by hash. A null
// result is reported as an error wrapping domain.ErrNotFound. A block without
// receipts returns an empty slice.
func (c *Client) BlockReceipts(ctx context.Context, hash domain.Hash) ([]Receipt, error) {
	var result *json.RawMessage
	if err := c.Call(ctx, "eth_getBlockReceipts", []any{hash.Hex()}, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, fmt.Errorf("rpc: eth_getBlockReceipts %s: %w", hash.Hex(), domain.ErrNotFound)
	}

	var payload []receiptPayload
	if err := json.Unmarshal(*result, &payload); err != nil {
		return nil, fmt.Errorf("rpc: eth_getBlockReceipts %s: decode receipts: %w", hash.Hex(), err)
	}

	receipts := make([]Receipt, len(payload))
	for i, raw := range payload {
		receipt, err := raw.receipt()
		if err != nil {
			return nil, fmt.Errorf("rpc: eth_getBlockReceipts %s: receipt %d: %w", hash.Hex(), i, err)
		}
		receipts[i] = receipt
	}
	return receipts, nil
}

type receiptPayload struct {
	TransactionHash *string      `json:"transactionHash"`
	Logs            []logPayload `json:"logs"`
}

func (p receiptPayload) receipt() (Receipt, error) {
	if p.TransactionHash == nil {
		return Receipt{}, fmt.Errorf("missing transactionHash")
	}
	transactionHash, err := domain.ParseHash(*p.TransactionHash)
	if err != nil {
		return Receipt{}, fmt.Errorf("parse transactionHash: %w", err)
	}

	logs := make([]Log, len(p.Logs))
	for i, raw := range p.Logs {
		log, err := raw.log()
		if err != nil {
			return Receipt{}, fmt.Errorf("log %d: %w", i, err)
		}
		// Receipt logs normally carry their own transaction hash; fill it from
		// the receipt when the node omits it.
		if log.TxHash == (domain.Hash{}) {
			log.TxHash = transactionHash
		}
		logs[i] = log
	}
	return Receipt{TransactionHash: transactionHash, Logs: logs}, nil
}

func decodeHexData(field, value string) ([]byte, error) {
	raw, ok := strings.CutPrefix(value, "0x")
	if !ok {
		return nil, fmt.Errorf("parse %s %q: missing 0x prefix", field, value)
	}
	data, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s %q: %w", field, value, err)
	}
	return data, nil
}
