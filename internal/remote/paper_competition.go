package remote

import (
	"context"
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

type paperCompetition struct {
	Basis        string                 `json:"basis"`
	Error        string                 `json:"error,omitempty"`
	Truncated    bool                   `json:"truncated"`
	Transactions []paperPoolTransaction `json:"transactions"`
}
type paperPoolTransaction struct {
	BidScenario *paperBidScenario   `json:"our_result_at_observed_tip,omitempty"`
	Hash        domain.Hash         `json:"hash"`
	Index       int                 `json:"index"`
	Sender      string              `json:"sender,omitempty"`
	Pools       []domain.PoolKey    `json:"pools"`
	GasUsed     uint64              `json:"gas_used"`
	PriorityFee *big.Int            `json:"effective_priority_fee,omitempty"`
	Fees        *domain.ReceiptFees `json:"paid_fees,omitempty"`
}

// Later pool activity is evidence of competition for state, not evidence that
// every touching transaction is an arbitrage or that our transaction would win.
func (c *paperCanonicalSource) competition(ctx context.Context, client *rpc.Client, prefix flashPrefix, opportunity arb.Opportunity) *paperCompetition {
	result := &paperCompetition{Basis: "canonical_later_pool_activity", Transactions: []paperPoolTransaction{}}
	for i := len(prefix.Transactions); i < len(c.calls); i++ {
		logs, err := c.calls[i].Result.logs()
		if err != nil {
			result.Error = err.Error()
			return result
		}
		var touched []domain.PoolKey
		for _, key := range opportunity.Cycle.Pools {
			for _, log := range logs {
				if log.Address != key.Address || key.IsV4() && (len(log.Topics) < 2 || log.Topics[1] != key.PoolID) {
					continue
				}
				touched = append(touched, key)
				break
			}
		}
		if len(touched) == 0 {
			continue
		}
		if len(result.Transactions) == 64 {
			result.Truncated = true
			break
		}
		gas, err := quantity(c.calls[i].Result.GasUsed)
		if err != nil {
			result.Error = err.Error()
			return result
		}
		result.Transactions = append(result.Transactions, paperPoolTransaction{Hash: c.block.transactions[i], Index: i, Sender: c.calls[i].Result.From, Pools: touched, GasUsed: gas})
	}
	if len(result.Transactions) == 0 {
		return result
	}
	if c.receipts == nil && c.receiptError == nil {
		h := c.block.header
		block := rpc.ExecutionBlock{Ref: c.block.ref, BaseFee: h.BaseFee, GasLimit: h.GasLimit, Coinbase: h.Coinbase, Transactions: c.block.transactions}
		c.receipts, c.receiptError = client.ExecutionReceipts(ctx, block, domain.FamilyOPStack)
	}
	if c.receiptError != nil {
		result.Error = c.receiptError.Error()
		return result
	}
	for i := range result.Transactions {
		tx := &result.Transactions[i]
		tx.Fees = c.receipts[tx.Index].Fees
		if tx.Fees != nil {
			tip := new(big.Int).Sub(tx.Fees.GasPrice, c.block.header.BaseFee)
			if tip.Sign() >= 0 {
				tx.PriorityFee = tip
			}
		}
	}
	return result
}
