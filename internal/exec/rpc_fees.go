package exec

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// NitroRPCFees uses the complete native charge observed in a remote state
// transition. It does not invent a poster/execution fee split absent from RPC.
// Nitro includes the DA charge inside the transaction's gas budget.
type NitroRPCFees struct{}

func (NitroRPCFees) Family() domain.ChainFamily                   { return domain.FamilyNitro }
func (NitroRPCFees) Budget(e ExecutionEvidence) (*big.Int, error) { return (NitroFees{}).Budget(e) }
func (NitroRPCFees) Validate(e ExecutionEvidence) error {
	if e.OP != nil || e.FeeAccounting != "nitro-rpc-total" || e.Context.ArbOSVersion == 0 || e.Context.ChainID == 0 || (e.Context.Family != "" && e.Context.Family != domain.FamilyNitro) || e.Context.Timestamp == 0 || e.Context.BlockGasLimit < e.Call.GasLimit || e.Owner == (domain.Address{}) || e.GasUsedForL1 != 0 || e.L1FeeWei != nil || e.L2FeeWei != nil || e.PaidGasPrice != nil || !e.L1FeeEstimated || e.Transaction != (domain.Hash{}) || e.GasUsed == 0 {
		return fmt.Errorf("incomplete remote Nitro total-fee evidence")
	}
	for _, field := range []struct {
		name     string
		value    *big.Int
		positive bool
	}{{"base fee", e.Context.BaseFee, true}, {"native fee", e.TotalFeeWei, true}, {"L1 price", e.Context.L1UnitPrice, false}} {
		if err := validUint(field.name, field.value, 256, field.positive); err != nil {
			return err
		}
	}
	budget, err := (NitroRPCFees{}).Budget(e)
	if err != nil || e.TotalFeeWei.Cmp(budget) > 0 {
		return fmt.Errorf("remote native charge exceeds envelope")
	}
	return nil
}
