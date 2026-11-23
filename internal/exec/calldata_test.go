package exec

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func encodingPlan(t *testing.T) ExecutorPlan {
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
	return ExecutorPlan{
		Funding: FundingV3Flash, Lender: domain.PoolKey{Kind: domain.PoolKindV3, Address: address("6")}, LenderToken0: true,
		Asset: address("4"), Amount: big.NewInt(1000), MaxLoanFee: big.NewInt(5),
		Steps: []ExecutorStep{
			{Pool: domain.PoolKey{Kind: domain.PoolKindV3, Address: address("1")}, TokenIn: address("4"), TokenOut: address("5"),
				Fee: 500, TickSpacing: 10, MinAmountOut: big.NewInt(1900)},
			{Pool: domain.PoolKey{Kind: domain.PoolKindV4, Address: address("2"), PoolID: id}, TokenIn: address("5"), TokenOut: address("4"),
				Fee: 3000, TickSpacing: 60, MinAmountOut: big.NewInt(1100), SqrtPriceLimitX96: big.NewInt(4295128740)},
		},
		MinProfit: big.NewInt(100), Deadline: 2_000_000_000,
	}
}

func TestExecutorEncodingMatchesSolidityVector(t *testing.T) {
	var vector struct {
		Calldata string `json:"calldata"`
		Hash     string `json:"hash"`
	}
	fixture, err := os.ReadFile("testdata/plan.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(fixture, &vector); err != nil {
		t.Fatal(err)
	}
	plan := encodingPlan(t)
	encoded, err := EncodeExecutorPlan(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := hex.DecodeString(strings.TrimPrefix(vector.Calldata, "0x"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded, expected) || keccak(encoded).Hex() != vector.Hash {
		t.Fatalf("calldata differs from cast/Solidity vector: %x", encoded)
	}
	plan.Amount.SetInt64(1)
	plan.Steps[0].MinAmountOut.SetInt64(1)
	if !bytes.Equal(encoded, expected) {
		t.Fatal("encoded calldata aliases caller-owned inputs")
	}
}

func TestExecutorEncodingRejectsInvalidBoundaryInputs(t *testing.T) {
	tests := []struct {
		name   string
		change func(*ExecutorPlan)
	}{
		{"funding", func(p *ExecutorPlan) { p.Funding = 4 }},
		{"no steps", func(p *ExecutorPlan) { p.Steps = nil }},
		{"many steps", func(p *ExecutorPlan) { p.Steps = make([]ExecutorStep, 17) }},
		{"no deadline", func(p *ExecutorPlan) { p.Deadline = 0 }},
		{"no amount", func(p *ExecutorPlan) { p.Amount = nil }},
		{"zero amount", func(p *ExecutorPlan) { p.Amount = new(big.Int) }},
		{"negative amount", func(p *ExecutorPlan) { p.Amount = big.NewInt(-1) }},
		{"overflow amount", func(p *ExecutorPlan) { p.Amount = new(big.Int).Lsh(big.NewInt(1), 256) }},
		{"zero profit", func(p *ExecutorPlan) { p.MinProfit = new(big.Int) }},
		{"nil loan fee", func(p *ExecutorPlan) { p.MaxLoanFee = nil }},
		{"negative loan fee", func(p *ExecutorPlan) { p.MaxLoanFee = big.NewInt(-1) }},
		{"wallet lender", func(p *ExecutorPlan) { p.Funding = FundingWallet }},
		{"wallet fee", func(p *ExecutorPlan) { p.Funding = FundingWallet; p.Lender, p.LenderToken0 = domain.PoolKey{}, false }},
		{"native flash", func(p *ExecutorPlan) { p.Asset = domain.Address{} }},
		{"non V3 lender", func(p *ExecutorPlan) { p.Lender.Kind = domain.PoolKindV2 }},
		{"zero target", func(p *ExecutorPlan) { p.Steps[0].Pool.Address = domain.Address{} }},
		{"unknown kind", func(p *ExecutorPlan) { p.Steps[0].Pool.Kind = domain.PoolKindWrap + 1 }},
		{"hooks", func(p *ExecutorPlan) { p.Steps[1].Pool.Hooks = p.Asset }},
		{"V4 no ID", func(p *ExecutorPlan) { p.Steps[1].Pool.PoolID = domain.Hash{} }},
		{"non V4 ID", func(p *ExecutorPlan) { p.Steps[0].Pool.PoolID = p.Steps[1].Pool.PoolID }},
		{"swap locked lender", func(p *ExecutorPlan) { p.Steps[0].Pool = p.Lender }},
		{"wrong first token", func(p *ExecutorPlan) { p.Steps[0].TokenIn = p.Steps[1].TokenIn }},
		{"disconnected leg", func(p *ExecutorPlan) { p.Steps[1].TokenIn = p.Steps[1].TokenOut }},
		{"same token out", func(p *ExecutorPlan) { p.Steps[0].TokenOut = p.Steps[0].TokenIn }},
		{"open route", func(p *ExecutorPlan) { p.Steps[1].TokenOut = p.Steps[0].TokenOut }},
		{"wallet lender direction", func(p *ExecutorPlan) {
			p.Funding, p.Lender, p.LenderToken0, p.MaxLoanFee = FundingWallet, domain.PoolKey{}, true, new(big.Int)
		}},
		{"fee width", func(p *ExecutorPlan) { p.Steps[1].Fee = 1 << 24 }},
		{"V2 fee", func(p *ExecutorPlan) { p.Steps[0].Pool.Kind = domain.PoolKindV2; p.Steps[0].Fee = 1_000_000 }},
		{"tick spacing width", func(p *ExecutorPlan) { p.Steps[1].TickSpacing = 1 << 23 }},
		{"native V3 output", func(p *ExecutorPlan) {
			p.Steps[0].TokenOut, p.Steps[1].TokenIn = domain.Address{}, domain.Address{}
		}},
		{"negative min out", func(p *ExecutorPlan) { p.Steps[0].MinAmountOut = big.NewInt(-1) }},
		{"nil min out", func(p *ExecutorPlan) { p.Steps[0].MinAmountOut = nil }},
		{"negative price", func(p *ExecutorPlan) { p.Steps[0].SqrtPriceLimitX96 = big.NewInt(-1) }},
		{"price overflow", func(p *ExecutorPlan) { p.Steps[0].SqrtPriceLimitX96 = new(big.Int).Lsh(big.NewInt(1), 160) }},
		{"V2 price limit", func(p *ExecutorPlan) {
			p.Steps[0].Pool.Kind = domain.PoolKindV2
			p.Steps[0].SqrtPriceLimitX96 = big.NewInt(1)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			plan := encodingPlan(t)
			test.change(&plan)
			if encoded, err := EncodeExecutorPlan(context.Background(), plan); err == nil || encoded != nil {
				t.Fatalf("invalid plan accepted: %x, %v", encoded, err)
			}
		})
	}
}

func TestWalletEncodingAndIntegerLimits(t *testing.T) {
	plan := encodingPlan(t)
	plan.Funding = FundingWallet
	plan.Lender, plan.LenderToken0 = domain.PoolKey{}, false
	plan.MaxLoanFee = new(big.Int)
	plan.Asset = domain.Address{}
	plan.Steps[0].Pool = plan.Steps[1].Pool
	plan.Steps[0].TokenIn = domain.Address{}
	plan.Steps[1].TokenOut = domain.Address{}
	plan.Steps[0].TickSpacing = -60
	plan.Amount = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
	plan.Steps[0].SqrtPriceLimitX96 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	encoded, err := EncodeExecutorPlan(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(encoded[164:196], bytes.Repeat([]byte{0xff}, 32)) {
		t.Fatal("uint256 amount truncated")
	}
	if !bytes.Equal(encoded[560:580], bytes.Repeat([]byte{0xff}, 20)) {
		t.Fatal("uint160 price truncated")
	}
	if !bytes.Equal(encoded[484:512], bytes.Repeat([]byte{0xff}, 28)) || new(big.Int).SetBytes(encoded[512:516]).Int64() != 0xffffffc4 {
		t.Fatal("negative int24 tick spacing is not two's complement")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := EncodeExecutorPlan(ctx, plan); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
}
