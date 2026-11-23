package exec

import (
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/rlp"

	"github.com/Rorical/Rosetta/internal/domain"
)

// OPFeeEvidence records hash-bound GasPriceOracle quotations. Unsigned L1 data
// is reserved at an upper bound; operator gas refunds use the measured gas.
type OPFeeEvidence struct {
	TraceMode             string
	OracleCodeHash        domain.Hash
	L1FeeUpperBound       *big.Int
	OperatorFee           *big.Int
	OperatorFeeUpperBound *big.Int
}

// OPFees keeps L1 data and operator charges outside the L2 execution gas budget.
type OPFees struct{}

func (OPFees) Family() domain.ChainFamily { return domain.FamilyOPStack }
func (OPFees) Budget(e ExecutionEvidence) (*big.Int, error) {
	gas, err := (NitroFees{}).Budget(e)
	if err != nil {
		return nil, err
	}
	if e.OP == nil {
		return nil, fmt.Errorf("OP fee quotation is missing")
	}
	for _, value := range []*big.Int{e.OP.L1FeeUpperBound, e.OP.OperatorFeeUpperBound} {
		if err := validUint("OP fee bound", value, 256, false); err != nil {
			return nil, err
		}
	}
	return gas.Add(gas, new(big.Int).Add(e.OP.L1FeeUpperBound, e.OP.OperatorFeeUpperBound)), nil
}
func (model OPFees) Validate(e ExecutionEvidence) error {
	if e.FeeAccounting != "op-rpc-reserved" || e.Context.Family != domain.FamilyOPStack || e.Context.ChainID == 0 || e.Context.ArbOSVersion != 0 || e.Context.Timestamp == 0 || e.Context.L1BlockNumber == 0 || e.Context.BlockGasLimit < e.Call.GasLimit || e.Owner == (domain.Address{}) || e.OP == nil || e.OP.OracleCodeHash == (domain.Hash{}) || e.GasUsed == 0 || e.GasUsedForL1 != 0 || !e.L1FeeEstimated || e.Transaction != (domain.Hash{}) {
		return fmt.Errorf("incomplete OP reserved-fee evidence")
	}
	if e.OP.TraceMode != "fee-free" && e.OP.TraceMode != "refund-only" {
		return fmt.Errorf("unsupported OP trace fee semantics")
	}
	for _, field := range []struct {
		name     string
		value    *big.Int
		positive bool
	}{
		{"base fee", e.Context.BaseFee, true}, {"L1 unit price", e.Context.L1UnitPrice, false}, {"paid gas price", e.PaidGasPrice, true},
		{"L1 fee", e.L1FeeWei, false}, {"L2 fee", e.L2FeeWei, false}, {"total fee", e.TotalFeeWei, true}, {"operator fee", e.OP.OperatorFee, false},
	} {
		if err := validUint(field.name, field.value, 256, field.positive); err != nil {
			return err
		}
	}
	budget, err := model.Budget(e)
	if err != nil {
		return err
	}
	if e.Call.TipCap == nil || e.Call.TipCap.Sign() < 0 {
		return fmt.Errorf("OP priority fee is missing")
	}
	price := new(big.Int).Add(e.Context.BaseFee, e.Call.TipCap)
	execution := new(big.Int).Mul(new(big.Int).SetUint64(e.GasUsed), price)
	total := new(big.Int).Add(execution, e.L1FeeWei)
	total.Add(total, e.OP.OperatorFee)
	if price.Cmp(e.PaidGasPrice) != 0 || price.Cmp(e.Call.FeeCap) > 0 || execution.Cmp(e.L2FeeWei) != 0 || e.L1FeeWei.Cmp(e.OP.L1FeeUpperBound) != 0 || e.OP.OperatorFee.Cmp(e.OP.OperatorFeeUpperBound) > 0 || total.Cmp(e.TotalFeeWei) != 0 || total.Cmp(budget) > 0 {
		return fmt.Errorf("OP fee components differ from quoted bounds and execution envelope")
	}
	return nil
}

// UnsignedDynamicFeeTx encodes the EIP-1559 signing payload without a signature.
// The calldata length is stable when the ABI's uint256 minProfit changes.
func UnsignedDynamicFeeTx(call ExecutorCall, chainID uint64) ([]byte, error) {
	if chainID == 0 || call.FeeCap == nil || call.TipCap == nil || call.Value == nil {
		return nil, fmt.Errorf("unsigned transaction context is incomplete")
	}
	payload, err := rlp.EncodeToBytes([]any{chainID, call.Nonce, call.TipCap, call.FeeCap, call.GasLimit, call.Executor, call.Value, call.Data, []any{}})
	if err != nil {
		return nil, fmt.Errorf("encode unsigned transaction: %w", err)
	}
	return append([]byte{2}, payload...), nil
}

// OPL1SizeBound deliberately exceeds GPO's practical 99.99% FastLZ bound.
// A signature adds at most 68 bytes; each literal needs at most one control
// byte per input byte and matches do not expand beyond their consumed input.
// Twice the maximum signed size therefore bounds even adversarial calldata.
func OPL1SizeBound(unsignedSize int) (uint64, error) {
	if unsignedSize <= 0 || unsignedSize > 1<<20 {
		return 0, fmt.Errorf("OP transaction size exceeds supported envelope")
	}
	return 2*(uint64(unsignedSize)+68) + 16, nil
}
