package exec

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// FeeModel validates native charging evidence and calculates a conservative
// native-currency budget. Backend-specific data fees must be included in both;
// execution gas alone is not a universal upper bound.
type FeeModel interface {
	Family() domain.ChainFamily
	Validate(ExecutionEvidence) error
	Budget(ExecutionEvidence) (*big.Int, error)
}

// NitroFees preserves ArbOS's charging of poster gas inside transaction gas.
// It deliberately cannot validate an OP Stack proof.
type NitroFees struct{}

func (NitroFees) Family() domain.ChainFamily { return domain.FamilyNitro }
func (NitroFees) Budget(e ExecutionEvidence) (*big.Int, error) {
	if e.Call.GasLimit == 0 || e.GasUsed > e.Call.GasLimit || e.Call.FeeCap == nil || e.Call.FeeCap.Sign() <= 0 {
		return nil, fmt.Errorf("execution gas differs from its budget")
	}
	return new(big.Int).Mul(new(big.Int).SetUint64(e.Call.GasLimit), e.Call.FeeCap), nil
}

func feeModel(e ExecutionEvidence, models []FeeModel) (FeeModel, error) {
	if len(models) > 1 {
		return nil, fmt.Errorf("one fee model is required")
	}
	var model FeeModel = NitroFees{}
	if e.FeeAccounting == "nitro-rpc-total" {
		model = NitroRPCFees{}
	}
	if e.FeeAccounting == "op-rpc-reserved" {
		model = OPFees{}
	}
	if len(models) == 1 {
		model = models[0]
	}
	family := e.Context.Family
	if family == "" {
		family = domain.FamilyNitro
	}
	if model == nil || model.Family() != family {
		return nil, fmt.Errorf("execution evidence and fee model belong to different chain families")
	}
	return model, nil
}

func (NitroFees) Validate(e ExecutionEvidence) error {
	if e.FeeAccounting != "" || e.OP != nil {
		return fmt.Errorf("native fee split requires native accounting evidence")
	}
	if e.Owner == (domain.Address{}) || e.Context.Timestamp == 0 || e.Context.ArbOSVersion == 0 ||
		e.Context.BlockGasLimit < e.Call.GasLimit || e.GasUsedForL1 > e.GasUsed {
		return fmt.Errorf("execution evidence has incomplete owner, context or gas data")
	}
	if e.L1FeeEstimated != (e.Transaction == (domain.Hash{})) {
		return fmt.Errorf("execution L1 fee mode differs from its signed transaction identity")
	}
	for _, field := range []struct {
		name     string
		value    *big.Int
		positive bool
	}{
		{"base fee", e.Context.BaseFee, true}, {"L1 unit price", e.Context.L1UnitPrice, false},
		{"paid gas price", e.PaidGasPrice, true}, {"L1 fee", e.L1FeeWei, false},
		{"L2 fee", e.L2FeeWei, false}, {"total fee", e.TotalFeeWei, false},
	} {
		if err := validUint(field.name, field.value, 256, field.positive); err != nil {
			return err
		}
	}
	if new(big.Int).Add(e.L1FeeWei, e.L2FeeWei).Cmp(e.TotalFeeWei) != 0 ||
		new(big.Int).Mul(new(big.Int).SetUint64(e.GasUsedForL1), e.PaidGasPrice).Cmp(e.L1FeeWei) != 0 ||
		e.PaidGasPrice.Cmp(e.Call.FeeCap) > 0 || new(big.Int).Mul(new(big.Int).SetUint64(e.GasUsed), e.PaidGasPrice).Cmp(e.TotalFeeWei) < 0 {
		return fmt.Errorf("execution fee components differ from charged gas and envelope")
	}
	return nil
}
