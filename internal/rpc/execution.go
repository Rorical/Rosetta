package rpc

import (
	"context"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ExecutionBlock includes the fields needed to bind remote simulations. It
// accepts OP headers, whose L1 context is held in predeploys rather than a
// l1BlockNumber header extension.
type ExecutionBlock struct {
	Ref          domain.BlockRef
	BaseFee      *big.Int
	GasLimit     uint64
	Coinbase     domain.Address
	Transactions []domain.Hash
}

func (c *Client) ExecutionBlock(ctx context.Context, tag any) (ExecutionBlock, error) {
	method := "eth_getBlockByNumber"
	requestedHash, byHash := tag.(domain.Hash)
	if byHash {
		method = "eth_getBlockByHash"
		tag = requestedHash.Hex()
	}
	var document *struct {
		Number        string    `json:"number"`
		Hash          string    `json:"hash"`
		ParentHash    string    `json:"parentHash"`
		Timestamp     string    `json:"timestamp"`
		BaseFee       string    `json:"baseFeePerGas"`
		GasLimit      string    `json:"gasLimit"`
		Miner         string    `json:"miner"`
		L1BlockNumber string    `json:"l1BlockNumber"`
		Transactions  *[]string `json:"transactions"`
	}
	if err := c.Call(ctx, method, []any{tag, false}, &document); err != nil {
		return ExecutionBlock{}, err
	}
	if document == nil {
		return ExecutionBlock{}, ErrBlockUnavailable
	}
	d := document
	number, err := parseQuantity("number", d.Number)
	if err != nil {
		return ExecutionBlock{}, err
	}
	hash, err := domain.ParseHash(d.Hash)
	if err != nil || hash == (domain.Hash{}) {
		return ExecutionBlock{}, fmt.Errorf("rpc: missing execution block hash")
	}
	if byHash && hash != requestedHash {
		return ExecutionBlock{}, fmt.Errorf("rpc: execution block differs from requested hash")
	}
	if text, ok := tag.(string); !byHash && ok && strings.HasPrefix(text, "0x") {
		requestedNumber, err := parseQuantity("requested number", text)
		if err != nil || requestedNumber != number {
			return ExecutionBlock{}, fmt.Errorf("rpc: execution block differs from requested height")
		}
	}
	parent, err := domain.ParseHash(d.ParentHash)
	if err != nil {
		return ExecutionBlock{}, err
	}
	timestamp, err := parseQuantity("timestamp", d.Timestamp)
	if err != nil {
		return ExecutionBlock{}, err
	}
	gas, err := parseQuantity("gasLimit", d.GasLimit)
	if err != nil || gas == 0 {
		return ExecutionBlock{}, fmt.Errorf("rpc: invalid execution gas limit")
	}
	fee, ok := new(big.Int).SetString(d.BaseFee, 0)
	if !ok || fee.Sign() <= 0 || fee.BitLen() > 256 {
		return ExecutionBlock{}, fmt.Errorf("rpc: invalid execution base fee")
	}
	coinbase, err := domain.ParseAddress(d.Miner)
	if err != nil {
		return ExecutionBlock{}, err
	}
	block := ExecutionBlock{Ref: domain.BlockRef{Number: number, Hash: hash, ParentHash: parent, Timestamp: timestamp}, BaseFee: fee, GasLimit: gas, Coinbase: coinbase}
	if d.L1BlockNumber != "" {
		block.Ref.L1Block, err = parseQuantity("l1BlockNumber", d.L1BlockNumber)
		if err != nil {
			return ExecutionBlock{}, err
		}
	}
	if d.Transactions == nil {
		return ExecutionBlock{}, fmt.Errorf("rpc: execution block missing transactions")
	}
	for _, text := range *d.Transactions {
		hash, err := domain.ParseHash(text)
		if err != nil {
			return ExecutionBlock{}, err
		}
		block.Transactions = append(block.Transactions, hash)
	}
	return block, nil
}

// ExecutionReceipts checks every receipt's block, index and transaction hash,
// including receipts without logs. This prevents missing or reordered effects.
func (c *Client) ExecutionReceipts(ctx context.Context, block ExecutionBlock, families ...domain.ChainFamily) ([]ExecutionReceipt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(block.Transactions) == 0 {
		return []ExecutionReceipt{}, nil
	}
	var records *[]struct {
		receiptFeePayload
		TransactionHash string             `json:"transactionHash"`
		From            string             `json:"from"`
		BlockHash       string             `json:"blockHash"`
		BlockNumber     string             `json:"blockNumber"`
		Index           string             `json:"transactionIndex"`
		Status          string             `json:"status"`
		Logs            *[]json.RawMessage `json:"logs"`
	}
	if err := c.Call(ctx, "eth_getBlockReceipts", []any{block.Ref.Hash.Hex()}, &records); err != nil {
		return nil, err
	}
	if records == nil || len(*records) != len(block.Transactions) {
		return nil, fmt.Errorf("rpc: incomplete block receipts")
	}
	result := make([]ExecutionReceipt, len(*records))
	var nextLog uint64
	for i, record := range *records {
		hash, err := domain.ParseHash(record.TransactionHash)
		if err != nil || hash != block.Transactions[i] || record.BlockHash != block.Ref.Hash.Hex() {
			return nil, fmt.Errorf("rpc: receipt identity differs from block")
		}
		number, err := parseQuantity("blockNumber", record.BlockNumber)
		if err != nil || number != block.Ref.Number {
			return nil, fmt.Errorf("rpc: receipt height mismatch")
		}
		index, err := parseQuantity("transactionIndex", record.Index)
		if err != nil || index != uint64(i) {
			return nil, fmt.Errorf("rpc: receipt order mismatch")
		}
		status, err := parseQuantity("status", record.Status)
		if err != nil || status > 1 || record.Logs == nil || status == 0 && len(*record.Logs) != 0 {
			return nil, fmt.Errorf("rpc: invalid receipt status/logs")
		}
		out := ExecutionReceipt{Hash: hash, Successful: status == 1}
		if record.From != "" {
			from, err := domain.ParseAddress(record.From)
			if err != nil {
				return nil, fmt.Errorf("rpc: invalid receipt sender")
			}
			out.From = &from
		}
		if len(families) > 0 {
			out.FeeParams = record.receiptFeePayload.feeParams(families[0])
			out.Fees, err = record.receiptFeePayload.fees(families[0])
			if out.Fees != nil && block.BaseFee != nil {
				out.Fees.BaseFee = new(big.Int).Set(block.BaseFee)
			}
			if err != nil {
				return nil, err
			}
		}
		for _, raw := range *record.Logs {
			var payload logPayload
			if err := json.Unmarshal(raw, &payload); err != nil {
				return nil, err
			}
			log, err := payload.log()
			if err != nil {
				return nil, err
			}
			if payload.Topics == nil || len(log.Topics) > 4 || uint64(log.LogIndex) != nextLog || payload.BlockNumber != nil && log.BlockNumber != block.Ref.Number || log.Removed || log.BlockHash != (domain.Hash{}) && log.BlockHash != block.Ref.Hash || log.TxHash != (domain.Hash{}) && log.TxHash != hash {
				return nil, fmt.Errorf("rpc: receipt log identity mismatch")
			}
			nextLog++
			out.Logs = append(out.Logs, log)
		}
		result[i] = out
	}
	return result, nil
}

type ExecutionReceipt struct {
	Fees *domain.ReceiptFees
	// FeeParams fingerprints the OP fee parameters of a user transaction; see
	// receiptFeePayload.feeParams. Empty for deposits and other families.
	FeeParams  string
	Hash       domain.Hash
	From       *domain.Address // nil when the node omits it
	Successful bool
	Logs       []Log
}

// BlockFeeParams returns the first fee fingerprint among a block's receipts,
// or "" when the block holds no user transaction that reports one.
func BlockFeeParams(receipts []ExecutionReceipt) string {
	for _, receipt := range receipts {
		if receipt.FeeParams != "" {
			return receipt.FeeParams
		}
	}
	return ""
}
