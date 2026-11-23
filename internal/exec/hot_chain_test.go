package exec

import (
	"encoding/hex"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// chainPlan: asset 0x01 -> 0x02 on a Uniswap V3 pool (1000 in, 2000 out),
// back on a Camelot V3 pool (2000 in, 1200 out).
func chainPlan() ExecutorPlan {
	plan := hotPlan()
	plan.Hot, plan.Chain = true, true
	plan.Steps[0].AmountIn, plan.Steps[0].AmountOut = big.NewInt(1000), big.NewInt(2000)
	plan.Steps[1].AmountIn, plan.Steps[1].AmountOut = big.NewInt(2000), big.NewInt(1200)
	return plan
}

// The layout HotV3Executor.loadRoute and checkBounds decode
// (contracts/test/ExecutorHotV3.t.sol builds the same words).
func TestHotChainEncoding(t *testing.T) {
	plan := chainPlan()
	encoded, err := EncodeHotChainPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	zeros := func(n int) string { return strings.Repeat("00", n) }
	address := func(first byte) string { return hex.EncodeToString([]byte{first}) + zeros(19) }
	payload := address(1) + zeros(14) + "03e8" + zeros(15) + "64" + "02" +
		"01" + zeros(11) + address(0xaa) + // 0x01 -> 0x02 is zeroForOne; asked for what the next pool is owed
		"00" + zeros(9) + "07d0" + address(0xbb) + // the outermost pool takes its 2000 in
		"81" + zeros(18) + "1234"
	selector := keccak([]byte("executeHot(bytes)"))
	want := hex.EncodeToString(selector[:4]) + zeros(31) + "20" + zeros(31) + "8a" + payload + zeros(22)
	if got := hex.EncodeToString(encoded); got != want {
		t.Fatalf("chained hot calldata\n got %s\nwant %s", got, want)
	}
	if len(encoded) > HotChainCalldataBound(2) {
		t.Fatalf("%d bytes above the bound %d", len(encoded), HotChainCalldataBound(2))
	}
	call, err := BuildExecutorCall(t.Context(), plan, CallParameters{Sender: domain.Address{7}, Executor: domain.Address{8}, CodeHash: domain.Hash{9}, GasLimit: 300_000, FeeCap: big.NewInt(2), TipCap: big.NewInt(1)})
	if err != nil || hex.EncodeToString(call.Data) != want {
		t.Fatal("a chained hot plan did not build executeHot calldata", err)
	}
}

// A pair after the outermost pool carries its output, a pair before it its
// input, and a Metric pool is the outermost.
func TestHotChainStepAmounts(t *testing.T) {
	a, b, c := domain.Address{1}, domain.Address{2}, domain.Address{3}
	step := func(kind domain.PoolKind, pool byte, in, out domain.Address, amountIn, amountOut int64) ExecutorStep {
		s := ExecutorStep{Pool: domain.PoolKey{Kind: kind, Address: domain.Address{pool}}, TokenIn: in, TokenOut: out, MinAmountOut: big.NewInt(1), AmountIn: big.NewInt(amountIn), AmountOut: big.NewInt(amountOut)}
		if kind == domain.PoolKindV2 {
			s.Fee = 3000
		}
		return s
	}
	plan := ExecutorPlan{Funding: FundingV3FlashSwap, Hot: true, Chain: true, Asset: a, Amount: big.NewInt(10), MaxLoanFee: new(big.Int), MinProfit: big.NewInt(1), Deadline: 1_900_000_000, Steps: []ExecutorStep{
		step(domain.PoolKindV3, 0xa1, a, b, 10, 20),
		step(domain.PoolKindV2, 0xa2, b, c, 20, 30),
		step(domain.PoolKindMetricOmm, 0xa3, c, b, 30, 40),
		step(domain.PoolKindV2, 0xa4, b, a, 40, 50),
	}}
	encoded, err := EncodeHotChainPlan(t.Context(), plan)
	if err != nil {
		t.Fatal(err)
	}
	words := encoded[68+53:]
	for i, want := range []struct {
		flags  byte
		amount int64
	}{{0x01, 0}, {0x03, 20}, {0x04, 30}, {0x02, 50}} {
		word := words[i*32 : (i+1)*32]
		if word[0] != want.flags || new(big.Int).SetBytes(word[1:12]).Int64() != want.amount || word[12] != plan.Steps[i].Pool.Address[0] {
			t.Fatalf("step %d: %x, want flags %#x amount %d", i, word, want.flags, want.amount)
		}
	}
}

func TestHotChainSupported(t *testing.T) {
	if !HotChainSupported(chainPlan()) {
		t.Fatal("V3 and Camelot route rejected")
	}
	metric := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{0xcc}}
	v3 := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{0xaa}}
	pair := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{0xdd}}
	if !HotChainPoolsSupported(FundingV3FlashSwap, []domain.PoolKey{v3, metric, pair}) {
		t.Fatal("a Metric pool as the outermost, followed by a pair, rejected")
	}
	for name, pools := range map[string][]domain.PoolKey{
		"a Metric pool before the outermost": {metric, v3},
		"the same pool twice":                {v3, pair, v3},
		"pairs alone":                        {pair, {Kind: domain.PoolKindV2, Address: domain.Address{0xde}}},
		"a Solidly V3 pool":                  {v3, {Kind: domain.PoolKindSolidlyV3, Address: domain.Address{0xdf}}},
	} {
		if HotChainPoolsSupported(FundingV3FlashSwap, pools) {
			t.Error(name, "accepted")
		}
	}
	for name, change := range map[string]func(*ExecutorPlan){
		"standalone loan":    func(p *ExecutorPlan) { p.Funding = FundingV3Flash },
		"no quoted amounts":  func(p *ExecutorPlan) { p.Steps[1].AmountIn = nil },
		"amount over 88 bit": func(p *ExecutorPlan) { p.Steps[1].AmountIn = new(big.Int).Lsh(big.NewInt(1), 88) },
		"cumulative check":   func(p *ExecutorPlan) { p.MinRate = big.NewInt(1) },
		"price limit":        func(p *ExecutorPlan) { p.Steps[0].SqrtPriceLimitX96 = big.NewInt(5) },
	} {
		plan := chainPlan()
		change(&plan)
		if HotChainSupported(plan) {
			t.Error(name, "accepted")
		}
	}
	// A pair cannot be checked by a price bound on this path.
	plan := chainPlan()
	plan.Steps[0].Pool.Kind, plan.Steps[0].Fee, plan.Steps[0].PriceBound = domain.PoolKindV2, 3000, big.NewInt(9)
	if _, err := EncodeHotChainPlan(t.Context(), plan); err == nil {
		t.Fatal("a bounded pair was encoded")
	}
}
