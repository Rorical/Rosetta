package exec

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func v4FlashPlan(t *testing.T) ExecutorPlan {
	t.Helper()
	plan := encodingPlan(t)
	plan.Funding, plan.Lender, plan.LenderToken0, plan.MaxLoanFee = FundingV4Flash, domain.PoolKey{}, false, new(big.Int)
	plan.Steps[0].Pool = plan.Steps[1].Pool
	plan.Steps[0].Pool.PoolID[0]++
	return plan
}

func TestV4FlashEncodingSendsNoPrincipalForERC20OrNative(t *testing.T) {
	for _, native := range []bool{false, true} {
		plan := v4FlashPlan(t)
		if native {
			plan.Asset, plan.Steps[0].TokenIn, plan.Steps[1].TokenOut = domain.Address{}, domain.Address{}, domain.Address{}
		}
		call, err := BuildExecutorCall(t.Context(), plan, callParameters())
		if err != nil {
			t.Fatal(err)
		}
		if call.Value.Sign() != 0 || call.Data[69] != byte(FundingV4Flash) {
			t.Fatal("V4 funding sent principal or encoded the wrong mode")
		}
		plan.Funding = FundingWallet
		wallet, err := BuildExecutorCall(t.Context(), plan, callParameters())
		if err != nil {
			t.Fatal(err)
		}
		wallet.Data[69] = byte(FundingV4Flash)
		if !bytes.Equal(wallet.Data, call.Data) || (native && wallet.Value.Cmp(plan.Amount) != 0) {
			t.Fatal("V4 funding changed the shared layout or native wallet funding")
		}
	}
}

func TestV4FlashEncodingRejectsLoansAndNonAtomicManagerRoutes(t *testing.T) {
	for name, change := range map[string]func(*ExecutorPlan){
		"lender":   func(p *ExecutorPlan) { p.Lender = p.Steps[0].Pool },
		"loan fee": func(p *ExecutorPlan) { p.MaxLoanFee.SetInt64(1) },
		"one step": func(p *ExecutorPlan) { p.Steps = p.Steps[:1] },
		"manager":  func(p *ExecutorPlan) { p.Steps[1].Pool.Address[0]++ },
		"first V3": func(p *ExecutorPlan) {
			p.Steps[0].Pool.Kind = domain.PoolKindV3
			p.Steps[0].Pool.PoolID = domain.Hash{}
		},
		"middle V3": func(p *ExecutorPlan) {
			p.Steps[1].Pool.Kind = domain.PoolKindV3
			p.Steps[1].Pool.PoolID = domain.Hash{}
		},
		"wrong input": func(p *ExecutorPlan) { p.Asset = domain.Address{} },
	} {
		t.Run(name, func(t *testing.T) {
			plan := v4FlashPlan(t)
			change(&plan)
			if encoded, err := EncodePackedExecutorPlan(t.Context(), plan); err == nil || encoded != nil {
				t.Fatal("invalid V4 funding accepted")
			}
		})
	}
}
