package exec

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// CallParameters bind an executor invocation to the actual operator and fee
// envelope. They do not authorize signing or submission.
type CallParameters struct {
	Sender   domain.Address
	Executor domain.Address
	CodeHash domain.Hash
	Nonce    uint64
	GasLimit uint64
	FeeCap   *big.Int
	TipCap   *big.Int
}

// ExecutorCall owns its calldata and integer values. A confirmation must check
// the deployed code hash and use this exact sender, nonce, value and envelope.
type ExecutorCall struct {
	CallParameters
	Value *big.Int
	Data  []byte
}

// BuildExecutorCall encodes the contract plan and derives native call value.
// This is an unsigned invocation, not a ready plan or a signed transaction.
func BuildExecutorCall(ctx context.Context, plan ExecutorPlan, parameters CallParameters) (ExecutorCall, error) {
	if parameters.Sender == (domain.Address{}) || parameters.Executor == (domain.Address{}) || parameters.CodeHash == (domain.Hash{}) {
		return ExecutorCall{}, fmt.Errorf("executor call requires sender, executor and code hash")
	}
	if parameters.GasLimit < 21_000 || parameters.GasLimit > 32_000_000 {
		return ExecutorCall{}, fmt.Errorf("executor gas limit must be between 21000 and 32000000")
	}
	if err := validUint("fee cap", parameters.FeeCap, 256, true); err != nil {
		return ExecutorCall{}, err
	}
	if err := validUint("tip cap", parameters.TipCap, 256, false); err != nil {
		return ExecutorCall{}, err
	}
	if parameters.TipCap.Cmp(parameters.FeeCap) > 0 {
		return ExecutorCall{}, fmt.Errorf("tip cap exceeds fee cap")
	}
	if plan.Claim && !plan.Hot {
		return ExecutorCall{}, fmt.Errorf("only a hot route claims an opportunity")
	}
	encode := EncodePackedExecutorPlan
	if plan.Hot {
		encode = EncodeHotV3Plan
		if plan.Chain {
			encode = EncodeHotChainPlan
		}
	}
	encoded, err := encode(ctx, plan)
	if err != nil {
		return ExecutorCall{}, err
	}
	parameters.FeeCap = new(big.Int).Set(parameters.FeeCap)
	parameters.TipCap = new(big.Int).Set(parameters.TipCap)
	value := new(big.Int)
	if plan.Funding == FundingWallet && plan.Asset == (domain.Address{}) {
		value.Set(plan.Amount)
	}
	return ExecutorCall{CallParameters: parameters, Value: value, Data: encoded}, nil
}
