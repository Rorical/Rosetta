package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Log is one EVM log entry returned by eth_getLogs or carried by a receipt.
//
// GetLogs fills every field. BlockReceipts parses the fields the node includes
// in each receipt log and falls back to the receipt's transaction hash when the
// log omits it; a receipt log without block metadata leaves BlockNumber and
// BlockHash zero.
type Log struct {
	Address     domain.Address
	Topics      []domain.Hash
	Data        []byte
	BlockNumber uint64
	BlockHash   domain.Hash
	TxHash      domain.Hash
	LogIndex    uint32
	Removed     bool
}

// LogFilter selects logs. Topics is a list of topic positions: entries within
// one position are ORed and positions are ANDed, so a nil position matches any
// topic at that index. An empty Addresses list matches every emitter.
type LogFilter struct {
	FromBlock uint64
	ToBlock   uint64
	Addresses []domain.Address
	Topics    [][]domain.Hash
}

// GetLogs returns the logs matching filter in the order the node reports them.
// A null result is an empty slice.
//
// Logs the node flags removed are skipped: a removed log belongs to a block a
// reorg has detached, and the caller re-queries the affected range instead of
// applying log data that no longer exists on the canonical chain. The
// BlockNumber and BlockHash fields of a returned log identify the block the
// node attributed it to.
//
// The block range is sent as given. Providers cap the number of results per
// query (Arbitrum public endpoints around 10,000), so callers that can span a
// large range must chunk it; this method does not retry a range that the node
// rejects as too wide.
func (c *Client) GetLogs(ctx context.Context, filter LogFilter) ([]Log, error) {
	if filter.FromBlock > filter.ToBlock {
		return nil, fmt.Errorf("rpc: eth_getLogs: from block %d is after to block %d",
			filter.FromBlock, filter.ToBlock)
	}

	var result *json.RawMessage
	if err := c.Call(ctx, "eth_getLogs", []any{filter.payload()}, &result); err != nil {
		return nil, err
	}
	if result == nil {
		return []Log{}, nil
	}

	var payload []logPayload
	if err := json.Unmarshal(*result, &payload); err != nil {
		return nil, fmt.Errorf("rpc: eth_getLogs: decode logs: %w", err)
	}

	logs := make([]Log, 0, len(payload))
	for i, raw := range payload {
		if raw.Removed {
			continue
		}
		log, err := raw.log()
		if err != nil {
			return nil, fmt.Errorf("rpc: eth_getLogs: log %d: %w", i, err)
		}
		logs = append(logs, log)
	}
	return logs, nil
}

// logFilterPayload is the eth_getLogs filter object. Empty address and topic
// lists are omitted rather than sent as null.
type logFilterPayload struct {
	FromBlock string     `json:"fromBlock"`
	ToBlock   string     `json:"toBlock"`
	Address   []string   `json:"address,omitempty"`
	Topics    [][]string `json:"topics,omitempty"`
}

func (f LogFilter) payload() logFilterPayload {
	payload := logFilterPayload{
		FromBlock: hexQuantity(f.FromBlock),
		ToBlock:   hexQuantity(f.ToBlock),
	}
	if len(f.Addresses) > 0 {
		payload.Address = make([]string, len(f.Addresses))
		for i, address := range f.Addresses {
			payload.Address[i] = address.Hex()
		}
	}
	if len(f.Topics) > 0 {
		payload.Topics = make([][]string, len(f.Topics))
		for i, position := range f.Topics {
			if position == nil {
				// A nil position stays nil and encodes as JSON null, the
				// wildcard for that topic index.
				continue
			}
			payload.Topics[i] = make([]string, len(position))
			for j, topic := range position {
				payload.Topics[i][j] = topic.Hex()
			}
		}
	}
	return payload
}

// logPayload uses pointers so a missing field is distinguishable from a zero
// value.
type logPayload struct {
	Address         *string  `json:"address"`
	Topics          []string `json:"topics"`
	Data            *string  `json:"data"`
	BlockNumber     *string  `json:"blockNumber"`
	BlockHash       *string  `json:"blockHash"`
	TransactionHash *string  `json:"transactionHash"`
	LogIndex        *string  `json:"logIndex"`
	Removed         bool     `json:"removed"`
}

func (p logPayload) log() (Log, error) {
	if p.Address == nil {
		return Log{}, fmt.Errorf("missing address")
	}
	if p.Data == nil {
		return Log{}, fmt.Errorf("missing data")
	}
	if p.LogIndex == nil {
		return Log{}, fmt.Errorf("missing logIndex")
	}

	address, err := domain.ParseAddress(*p.Address)
	if err != nil {
		return Log{}, fmt.Errorf("parse address: %w", err)
	}
	data, err := decodeHexData("data", *p.Data)
	if err != nil {
		return Log{}, err
	}
	index, err := parseQuantity("logIndex", *p.LogIndex)
	if err != nil {
		return Log{}, err
	}
	if index > math.MaxUint32 {
		return Log{}, fmt.Errorf("parse logIndex %q: value does not fit in uint32", *p.LogIndex)
	}

	topics := make([]domain.Hash, len(p.Topics))
	for i, topic := range p.Topics {
		parsed, err := domain.ParseHash(topic)
		if err != nil {
			return Log{}, fmt.Errorf("parse topic %d: %w", i, err)
		}
		topics[i] = parsed
	}

	var blockNumber uint64
	if p.BlockNumber != nil {
		blockNumber, err = parseQuantity("blockNumber", *p.BlockNumber)
		if err != nil {
			return Log{}, err
		}
	}
	var blockHash, txHash domain.Hash
	if p.BlockHash != nil {
		blockHash, err = domain.ParseHash(*p.BlockHash)
		if err != nil {
			return Log{}, fmt.Errorf("parse blockHash: %w", err)
		}
	}
	if p.TransactionHash != nil {
		txHash, err = domain.ParseHash(*p.TransactionHash)
		if err != nil {
			return Log{}, fmt.Errorf("parse transactionHash: %w", err)
		}
	}

	return Log{
		Address:     address,
		Topics:      topics,
		Data:        data,
		BlockNumber: blockNumber,
		BlockHash:   blockHash,
		TxHash:      txHash,
		LogIndex:    uint32(index),
		Removed:     p.Removed,
	}, nil
}
