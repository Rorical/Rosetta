package domain

import "math/big"

// ReceiptFees contains paid native-currency charges from a canonical receipt.
// Nitro gasUsed already includes its L1 charge. OP adds L1 and operator fees.
// A nil record means unavailable; omitted components are never invented.
type ReceiptFees struct {
	BaseFee   *big.Int    `json:"block_base_fee,omitempty"`
	Family    ChainFamily `json:"family"`
	GasUsed   uint64      `json:"gas_used"`
	GasPrice  *big.Int    `json:"effective_gas_price"`
	Execution *big.Int    `json:"execution_fee"`
	L1        *big.Int    `json:"l1_fee,omitempty"`
	Operator  *big.Int    `json:"operator_fee,omitempty"`
	Total     *big.Int    `json:"total_fee"`
}

// PriorityFee returns the paid price above the canonical block base fee.
// Old captures without header fee context remain unknown.
func (f *ReceiptFees) PriorityFee() *big.Int {
	if f == nil || f.BaseFee == nil || f.GasPrice == nil {
		return nil
	}
	tip := new(big.Int).Sub(f.GasPrice, f.BaseFee)
	if tip.Sign() < 0 {
		return nil
	}
	return tip
}
