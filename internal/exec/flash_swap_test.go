package exec

import (
	"bytes"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func flashSwapPlan(t *testing.T) ExecutorPlan {
	t.Helper()
	plan := encodingPlan(t)
	plan.Funding, plan.Lender, plan.LenderToken0, plan.MaxLoanFee = FundingV3FlashSwap, domain.PoolKey{}, false, new(big.Int)
	return plan
}

func TestFlashSwapEncodingPreservesPackedLayoutAndSendsNoPrincipal(t *testing.T) {
	plan := flashSwapPlan(t)
	call, err := BuildExecutorCall(t.Context(), plan, callParameters())
	if err != nil {
		t.Fatal(err)
	}
	if call.Value.Sign() != 0 || call.Data[69] != byte(FundingV3FlashSwap) {
		t.Fatal("wrong flash swap value or funding word")
	}
	plan.Funding = FundingWallet
	wallet, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// Both modes omit the standalone lender and loan fee.
	wallet[69] = byte(FundingV3FlashSwap)
	if !bytes.Equal(call.Data, wallet) {
		t.Fatal("flash swap changed the shared packed layout")
	}
}

func TestFlashSwapEncodingRejectsUnsupportedOrLockedRoutes(t *testing.T) {
	for name, change := range map[string]func(*ExecutorPlan){
		"lender":             func(p *ExecutorPlan) { p.Lender = p.Steps[0].Pool },
		"extra loan fee":     func(p *ExecutorPlan) { p.MaxLoanFee.SetInt64(1) },
		"native principal":   func(p *ExecutorPlan) { p.Asset = domain.Address{} },
		"V2 first step":      func(p *ExecutorPlan) { p.Steps[0].Pool.Kind = domain.PoolKindV2 },
		"Algebra first step": func(p *ExecutorPlan) { p.Steps[0].Pool.Kind = domain.PoolKindCamelotV3 },
		"one step":           func(p *ExecutorPlan) { p.Steps = p.Steps[:1] },
		"locked first pool":  func(p *ExecutorPlan) { p.Steps[1].Pool = p.Steps[0].Pool },
	} {
		t.Run(name, func(t *testing.T) {
			plan := flashSwapPlan(t)
			change(&plan)
			if encoded, err := EncodeExecutorPlan(t.Context(), plan); err == nil || encoded != nil {
				t.Fatal("invalid flash swap plan accepted")
			}
		})
	}
}

func TestSolidlyFirstLegCanFundAFlashSwap(t *testing.T) {
	plan := flashSwapPlan(t)
	plan.Steps[0].Pool.Kind = domain.PoolKindSolidlyV3
	encoded, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if encoded[68+95] != byte(domain.PoolKindSolidlyV3) {
		t.Fatal("Solidly kind byte not encoded")
	}
}
