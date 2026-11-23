package exec

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// wrapPlan is a wallet-funded route in the wrapped native token (the 4
// address): a V3 pool, a V4 pool that pays native currency, and the deposit
// that wraps it. contracts/test/ExecutorEncoding.t.sol builds the same plan
// and pins the same hashes.
func wrapPlan(t *testing.T) ExecutorPlan {
	t.Helper()
	address := func(digit string) domain.Address {
		value, err := domain.ParseAddress(strings.Repeat(digit, 40))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	id, err := domain.ParseHash(strings.Repeat("33", 32))
	if err != nil {
		t.Fatal(err)
	}
	wrapped, native := address("4"), domain.Address{}
	return ExecutorPlan{
		Funding: FundingWallet, Asset: wrapped, Amount: big.NewInt(1000), MaxLoanFee: new(big.Int),
		Steps: []ExecutorStep{
			{Pool: domain.PoolKey{Kind: domain.PoolKindV3, Address: address("1")}, TokenIn: wrapped, TokenOut: address("5"), Fee: 500, TickSpacing: 10, MinAmountOut: big.NewInt(1900)},
			{Pool: domain.PoolKey{Kind: domain.PoolKindV4, Address: address("2"), PoolID: id}, TokenIn: address("5"), TokenOut: native, Fee: 3000, TickSpacing: 60, MinAmountOut: big.NewInt(1100)},
			{Pool: domain.PoolKey{Kind: domain.PoolKindWrap, Address: wrapped}, TokenIn: native, TokenOut: wrapped, MinAmountOut: big.NewInt(1100)},
		},
		MinProfit: big.NewInt(100), Deadline: 2_000_000_000,
	}
}

// unwrapPlan is wrapPlan run the other way: the withdrawal first.
func unwrapPlan(t *testing.T) ExecutorPlan {
	plan := wrapPlan(t)
	wrapped, middle, native := plan.Asset, plan.Steps[0].TokenOut, domain.Address{}
	deposit, v4, v3 := plan.Steps[2], plan.Steps[1], plan.Steps[0]
	deposit.TokenIn, deposit.TokenOut = wrapped, native
	v4.TokenIn, v4.TokenOut = native, middle
	v3.TokenIn, v3.TokenOut = middle, wrapped
	plan.Steps = []ExecutorStep{deposit, v4, v3}
	return plan
}

const (
	wrapPlanHash       = "0xd2b76c4fb05c46932f61bd24be7097ca3a5e669fa9a2e28077a1eb19a9f7141c"
	wrapPackedPlanHash = "0x0378bf18cdf12a655c51a5f6288c94d7137200829d8632fa30a495d5804a0f40"
)

// The executor's WRAP leg is kind 9: its target is the wrapped native token,
// which is also the side that is not native currency, and it carries the
// leg's minimum and nothing else. Both encodings must carry exactly that.
func TestWrapStepEncodesAsKindNine(t *testing.T) {
	plan := wrapPlan(t)
	encoded, err := EncodeExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// ABI: selector, offset, nine head words, the step count, then seven
	// words per step: kind, target, tokenOut, fee, tickSpacing, minimum, limit.
	step := encoded[356+2*224 : 356+3*224]
	word := func(i int) *big.Int { return new(big.Int).SetBytes(step[i*32 : (i+1)*32]) }
	if word(0).Int64() != 9 || domain.Address(step[44:64]) != plan.Asset || domain.Address(step[76:96]) != plan.Asset {
		t.Fatalf("step does not name a kind 9 deposit into the wrapped token: %x", step)
	}
	if word(3).Sign() != 0 || word(4).Sign() != 0 || word(5).Int64() != 1100 || word(6).Sign() != 0 {
		t.Fatalf("fee, spacing, minimum or limit differ: %x", step)
	}
	if got := keccak(encoded).Hex(); got != wrapPlanHash {
		t.Fatalf("ABI plan hash %s; update it here and in ExecutorEncoding.t.sol together", got)
	}

	packed, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// Packed: 68-byte ABI head, 95-byte header, then 80 bytes per step
	// ending in the flags byte; no flag means neither limit nor bound.
	last := packed[68+95+160 : 68+95+240]
	if packed[68] != 2 || last[0] != 9 || domain.Address(last[1:21]) != plan.Asset || domain.Address(last[21:41]) != plan.Asset || new(big.Int).SetBytes(last[47:79]).Int64() != 1100 || last[79] != 0 {
		t.Fatalf("packed step differs: %x", last)
	}
	if got := keccak(packed).Hex(); got != wrapPackedPlanHash {
		t.Fatalf("packed plan hash %s; update it here and in ExecutorEncoding.t.sol together", got)
	}

	// The withdrawal names the same target with native currency as its output.
	packed, err = EncodePackedExecutorPlan(t.Context(), unwrapPlan(t))
	if err != nil {
		t.Fatal(err)
	}
	first := packed[68+95 : 68+95+80]
	if first[0] != 9 || domain.Address(first[1:21]) != plan.Asset || domain.Address(first[21:41]) != (domain.Address{}) {
		t.Fatalf("packed withdrawal differs: %x", first)
	}
}

// A wrap leg converts between native currency and its own target only, has
// no price to limit or bound, cannot lend and never runs on a hot path.
func TestWrapStepIsBoundToItsTokenAndStaysOutOfOtherPaths(t *testing.T) {
	other := domain.Address{9}
	cases := map[string]func(*ExecutorPlan){
		"target is not the wrapped side": func(p *ExecutorPlan) { p.Steps[2].Pool.Address = other },
		"no native side": func(p *ExecutorPlan) {
			p.Steps[1].TokenOut, p.Steps[2].TokenIn = other, other
		},
		"pool ID":     func(p *ExecutorPlan) { p.Steps[2].Pool.PoolID = domain.Hash{1} },
		"hooks":       func(p *ExecutorPlan) { p.Steps[2].Pool.Hooks = other },
		"fee":         func(p *ExecutorPlan) { p.Steps[2].Fee = 1 },
		"spacing":     func(p *ExecutorPlan) { p.Steps[2].TickSpacing = 1 },
		"price limit": func(p *ExecutorPlan) { p.Steps[2].SqrtPriceLimitX96 = big.NewInt(4295128740) },
		"price bound": func(p *ExecutorPlan) { p.Steps[2].PriceBound = big.NewInt(1) },
		"native V3":   func(p *ExecutorPlan) { p.Steps[2].Pool.Kind = domain.PoolKindV3 },
		"V4 flash":    func(p *ExecutorPlan) { p.Funding = FundingV4Flash },
	}
	for name, mutate := range cases {
		plan := wrapPlan(t)
		mutate(&plan)
		if _, err := EncodePackedExecutorPlan(t.Context(), plan); err == nil {
			t.Errorf("%s: plan accepted", name)
		}
	}
	// The withdrawal cannot start a flash swap; the V3 leg of the deposit route can.
	plan := unwrapPlan(t)
	plan.Funding = FundingV3FlashSwap
	if _, err := EncodePackedExecutorPlan(t.Context(), plan); err == nil {
		t.Error("flash swap started on a wrap leg")
	}
	plan = wrapPlan(t)
	plan.Funding = FundingV3FlashSwap
	if _, err := EncodePackedExecutorPlan(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if HotRouteSupported(plan) || HotChainSupported(plan) {
		t.Error("hot path accepted a wrap leg")
	}
	pools := []domain.PoolKey{plan.Steps[0].Pool, plan.Steps[2].Pool}
	if HotPoolsSupported(plan.Funding, pools) || HotChainPoolsSupported(plan.Funding, pools) {
		t.Error("hot path accepted a wrap pool")
	}
	if _, err := EncodeHotV3Plan(t.Context(), plan); err == nil {
		t.Error("hot encoding accepted a wrap leg")
	}
	if _, err := EncodeHotChainPlan(t.Context(), plan); err == nil {
		t.Error("chained hot encoding accepted a wrap leg")
	}
	// A bound on the other legs is version 3 and leaves the wrap leg unflagged.
	plan = wrapPlan(t)
	plan.Steps[0].PriceBound, plan.Steps[1].PriceBound = big.NewInt(1<<40), big.NewInt(1<<40)
	packed, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if packed[68] != 3 || packed[68+95+79] != 2 || packed[68+95+100+79] != 2 || packed[68+95+200+79] != 0 {
		t.Fatalf("bounds differ: %x", packed)
	}
}
