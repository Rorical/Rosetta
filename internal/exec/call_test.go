package exec

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func callParameters() CallParameters {
	return CallParameters{Sender: domain.Address{1}, Executor: domain.Address{2}, CodeHash: domain.Hash{3},
		GasLimit: 1_000_000, FeeCap: big.NewInt(100), TipCap: big.NewInt(2), Nonce: 4}
}

func TestExecutorCallOwnsEnvelopeAndDerivesValue(t *testing.T) {
	plan := encodingPlan(t)
	parameters := callParameters()
	call, err := BuildExecutorCall(t.Context(), plan, parameters)
	if err != nil {
		t.Fatal(err)
	}
	parameters.FeeCap.SetInt64(1)
	parameters.TipCap.SetInt64(1)
	if call.FeeCap.Int64() != 100 || call.TipCap.Int64() != 2 || call.Value.Sign() != 0 || call.Nonce != 4 {
		t.Fatal("call aliases parameters or has wrong value/nonce")
	}
	plan.Funding, plan.Lender, plan.LenderToken0, plan.MaxLoanFee = FundingWallet, domain.PoolKey{}, false, new(big.Int)
	plan.Asset, plan.Steps[0].TokenIn, plan.Steps[1].TokenOut = domain.Address{}, domain.Address{}, domain.Address{}
	plan.Steps[0].Pool = plan.Steps[1].Pool
	call, err = BuildExecutorCall(t.Context(), plan, callParameters())
	if err != nil || call.Value.Cmp(plan.Amount) != 0 {
		t.Fatalf("native wallet value: %v %v", call.Value, err)
	}
	plan.Amount.SetInt64(1)
	if call.Value.Int64() != 1000 {
		t.Fatal("native value aliases plan amount")
	}
}

func TestExecutorCallRejectsInvalidEnvelope(t *testing.T) {
	for name, change := range map[string]func(*CallParameters){
		"sender":          func(p *CallParameters) { p.Sender = domain.Address{} },
		"executor":        func(p *CallParameters) { p.Executor = domain.Address{} },
		"code hash":       func(p *CallParameters) { p.CodeHash = domain.Hash{} },
		"low gas":         func(p *CallParameters) { p.GasLimit = 20_999 },
		"high gas":        func(p *CallParameters) { p.GasLimit = 32_000_001 },
		"fee missing":     func(p *CallParameters) { p.FeeCap = nil },
		"fee overflow":    func(p *CallParameters) { p.FeeCap = new(big.Int).Lsh(big.NewInt(1), 256) },
		"tip missing":     func(p *CallParameters) { p.TipCap = nil },
		"tip exceeds fee": func(p *CallParameters) { p.TipCap.SetInt64(101) },
	} {
		t.Run(name, func(t *testing.T) {
			parameters := callParameters()
			change(&parameters)
			if _, err := BuildExecutorCall(context.Background(), encodingPlan(t), parameters); err == nil {
				t.Fatal("invalid call envelope accepted")
			}
		})
	}
}
