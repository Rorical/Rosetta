package rpc

import (
	"context"
	"fmt"
	"github.com/Rorical/Rosetta/internal/domain"
)

// TransactionReceipt retains paid fees and block identity for settlement.
type TransactionReceipt struct {
	Hash    domain.Hash
	Block   domain.BlockRef
	Success bool
	Logs    []Log
	Fees    *domain.ReceiptFees
}

func (c *Client) TransactionReceipt(ctx context.Context, hash domain.Hash, family domain.ChainFamily) (*TransactionReceipt, error) {
	var p *struct {
		receiptPayload
		receiptFeePayload
		BlockHash   string `json:"blockHash"`
		BlockNumber string `json:"blockNumber"`
		Status      string `json:"status"`
	}
	if err := c.Call(ctx, "eth_getTransactionReceipt", []any{hash.Hex()}, &p); err != nil || p == nil {
		return nil, err
	}
	base, err := p.receipt()
	if err != nil {
		return nil, err
	}
	if base.TransactionHash != hash {
		return nil, fmt.Errorf("receipt transaction mismatch")
	}
	h, err := domain.ParseHash(p.BlockHash)
	if err != nil || h == (domain.Hash{}) {
		return nil, fmt.Errorf("missing receipt block hash")
	}
	n, err := parseQuantity("receipt block", p.BlockNumber)
	if err != nil {
		return nil, err
	}
	status, err := parseQuantity("receipt status", p.Status)
	if err != nil || status > 1 {
		return nil, fmt.Errorf("invalid receipt status")
	}
	fees, err := p.fees(family)
	if err != nil {
		return nil, err
	}
	return &TransactionReceipt{Hash: hash, Block: domain.BlockRef{Number: n, Hash: h}, Success: status == 1, Logs: base.Logs, Fees: fees}, nil
}
