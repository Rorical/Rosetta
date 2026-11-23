package exec

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// metricPlan is a wallet-funded two-hop route, MetricOmm then V3, on
// synthetic addresses. contracts/test/ExecutorEncoding.t.sol builds the same
// plan and pins the same hashes.
func metricPlan(t *testing.T) ExecutorPlan {
	t.Helper()
	address := func(digit string) domain.Address {
		value, err := domain.ParseAddress(strings.Repeat(digit, 40))
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	return ExecutorPlan{
		Funding: FundingWallet, Asset: address("4"), Amount: big.NewInt(1000), MaxLoanFee: new(big.Int),
		Steps: []ExecutorStep{
			{Pool: domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: address("7")}, TokenIn: address("4"), TokenOut: address("5"), MinAmountOut: big.NewInt(1900)},
			{Pool: domain.PoolKey{Kind: domain.PoolKindV3, Address: address("1")}, TokenIn: address("5"), TokenOut: address("4"), Fee: 500, TickSpacing: 10, MinAmountOut: big.NewInt(1100)},
		},
		MinProfit: big.NewInt(100), Deadline: 2_000_000_000,
	}
}

const (
	metricPlanHash       = "0x18173699c2cc0343068d297c5b70ab714df179f73cf21ac0cb6b8bcb30e41a6b"
	metricPackedPlanHash = "0x5fc1814129150e95761d030d4d968a1773abf3a9a3001548d6cf3820af160546"
)

// The executor's MetricOmm leg is kind 8 with a zero price limit and the
// leg's own minimum. Both encodings must carry exactly that, and decode to it.
func TestMetricOmmStepEncodesAsKindEightWithoutLimit(t *testing.T) {
	plan := metricPlan(t)
	encoded, err := EncodeExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// ABI: selector, offset, nine head words, the step count, then seven
	// words per step: kind, target, tokenOut, fee, tickSpacing, minimum, limit.
	step := encoded[356 : 356+224]
	word := func(i int) *big.Int { return new(big.Int).SetBytes(step[i*32 : (i+1)*32]) }
	if word(0).Int64() != 8 || domain.Address(step[44:64]) != plan.Steps[0].Pool.Address || domain.Address(step[76:96]) != plan.Steps[0].TokenOut {
		t.Fatalf("step does not name a kind 8 leg: %x", step)
	}
	if word(3).Sign() != 0 || word(4).Sign() != 0 || word(5).Int64() != 1900 || word(6).Sign() != 0 {
		t.Fatalf("fee, spacing, minimum or limit differ: %x", step)
	}
	if got := keccak(encoded).Hex(); got != metricPlanHash {
		t.Fatalf("ABI plan hash %s; update it here and in ExecutorEncoding.t.sol together", got)
	}

	packed, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	// Packed: 68-byte ABI head, 95-byte header, then 80 bytes per step
	// ending in the flags byte; no flag means neither limit nor bound.
	first := packed[68+95 : 68+95+80]
	if packed[68] != 2 || first[0] != 8 || domain.Address(first[1:21]) != plan.Steps[0].Pool.Address || new(big.Int).SetBytes(first[47:79]).Int64() != 1900 || first[79] != 0 {
		t.Fatalf("packed step differs: %x", first)
	}
	if got := keccak(packed).Hex(); got != metricPackedPlanHash {
		t.Fatalf("packed plan hash %s; update it here and in ExecutorEncoding.t.sol together", got)
	}
}

// Everything that reads a V3 price stays closed to the kind: the contract
// rejects a limit on the leg, PriceCheck would pass a bound unchecked, the
// flash-swap callback and the standalone loan are V3 pools, and the hot path
// takes V3-family routes only.
func TestMetricOmmStaysOutOfV3OnlyPaths(t *testing.T) {
	cases := map[string]func(*ExecutorPlan){
		"price limit":      func(p *ExecutorPlan) { p.Steps[0].SqrtPriceLimitX96 = big.NewInt(4295128740) },
		"price bound":      func(p *ExecutorPlan) { p.Steps[0].PriceBound = big.NewInt(1) },
		"flash swap first": func(p *ExecutorPlan) { p.Funding = FundingV3FlashSwap },
		"flash lender": func(p *ExecutorPlan) {
			p.Funding, p.MaxLoanFee = FundingV3Flash, big.NewInt(1)
			p.Lender = domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{9}}
		},
		"V4 flash": func(p *ExecutorPlan) { p.Funding = FundingV4Flash },
	}
	for name, mutate := range cases {
		plan := metricPlan(t)
		mutate(&plan)
		if _, err := EncodePackedExecutorPlan(t.Context(), plan); err == nil {
			t.Errorf("%s: plan accepted", name)
		}
	}
	plan := metricPlan(t)
	// A bound on the V3 leg alone is version 3 and leaves the MetricOmm leg unflagged.
	plan.Steps[1].PriceBound = big.NewInt(1 << 40)
	packed, err := EncodePackedExecutorPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if packed[68] != 3 || packed[68+95+79] != 0 || packed[68+95+80+79] != 2 {
		t.Fatalf("bounds differ: %x", packed)
	}
	// Flash-swap funding with the V3 leg first is allowed, but not hot.
	plan = metricPlan(t)
	plan.Funding = FundingV3FlashSwap
	plan.Steps[0], plan.Steps[1] = plan.Steps[1], plan.Steps[0]
	plan.Steps[0].TokenIn, plan.Steps[0].TokenOut, plan.Steps[1].TokenIn, plan.Steps[1].TokenOut = plan.Asset, plan.Steps[0].TokenIn, plan.Steps[0].TokenIn, plan.Asset
	if _, err := EncodePackedExecutorPlan(t.Context(), plan); err != nil {
		t.Fatal(err)
	}
	if HotRouteSupported(plan) {
		t.Fatal("hot path accepted a MetricOmm leg")
	}
	if _, err := EncodeHotV3Plan(t.Context(), plan); err == nil {
		t.Fatal("hot encoding accepted a MetricOmm leg")
	}
}
