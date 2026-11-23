package planning

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

type transactionView struct{ fundedView }

func (transactionView) Deep(domain.PoolKey) (pool.DeepState, bool) {
	return pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}, WindowMin: -2000, WindowMax: 2000}, true
}

func transactionFixture() (transactionView, arb.Opportunity) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := transactionView{fundedView{
		p: {Key: p, Token0: a, Token1: b, Fee: 100, TickSpacing: 10, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1e18), Slipstream: &pool.SlipstreamState{Valid: true, BlockFee: 100, SpacingFee: 100, TaxModule: domain.Address{5}, TaxMinimum: "100", TaxBaseFactor: "2", TaxMultiplier: 1_000_000_000_000}},
		q: {Key: q, Token0: a, Token1: b, Fee: 3000, Reserve0: big.NewInt(1_080_000_000_000_000_000), Reserve1: big.NewInt(1e18)},
	}}
	return v, arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1e12)}
}

func TestTransactionQuoteTaxRepricesEveryLegWithoutMutatingState(t *testing.T) {
	v, o := transactionFixture()
	zero, _, err := QuoteTransaction(t.Context(), v, o, big.NewInt(100), new(big.Int), 0)
	if err != nil {
		t.Fatal(err)
	}
	taxed, fees, err := QuoteTransaction(t.Context(), v, o, big.NewInt(100), big.NewInt(203), 0)
	if err != nil {
		t.Fatal(err)
	}
	if fees[0].FeePips != 103 || taxed.Net.Cmp(zero.Net) >= 0 || taxed.Legs[1].AmountIn.Cmp(taxed.Legs[0].AmountOut) != 0 {
		t.Fatal(taxed, fees)
	}
	bounded, boundFees, err := QuoteTransactionBound(t.Context(), v, o, big.NewInt(203))
	if err != nil || boundFees[0].FeePips != 203 || bounded.Net.Cmp(taxed.Net) > 0 {
		t.Fatal(boundFees, err)
	}
	p := v.fundedView[o.Cycle.Pools[0]]
	if p.Fee != 100 || p.Slipstream.BlockFee != 100 {
		t.Fatal("shared state mutated")
	}
	p.Slipstream.Valid = false
	if _, _, err = QuoteTransactionBound(t.Context(), v, o, big.NewInt(203)); err == nil {
		t.Fatal("invalid oracle admitted")
	}
}

func TestReactiveBidRequotesTaxAndReplacementCannotReuseOldFee(t *testing.T) {
	v, original := transactionFixture()
	o, _, err := QuoteTransaction(t.Context(), v, original, big.NewInt(100), new(big.Int), 0)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(1), Extra: new(big.Int), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 100000, MaxFee: big.NewInt(1_000_000), MaxTip: big.NewInt(100000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 10000, SlippageBPS: 20, Lifetime: 500 * time.Millisecond}
	version := domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{2}}
	conversion := execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}
	intent, err := BuildReactive(t.Context(), v, o, nil, o.Input, version, conversion, costs, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if intent.Call.TipCap.Int64() != 50000 {
		t.Fatalf("expected tax-limited bid 50000, got %s", intent.Call.TipCap)
	}
	repriced, _, err := QuoteTransactionBound(t.Context(), v, o, intent.Call.TipCap)
	if err != nil || intent.QuotedGross.Cmp(repriced.Net) != 0 || intent.QuotedGross.Cmp(o.Net) >= 0 {
		t.Fatal(intent.QuotedGross, err)
	}
	cfg.MinimumTip = big.NewInt(100000)
	if _, err = BuildReactive(t.Context(), v, o, nil, o.Input, version, conversion, costs, cfg, now); err == nil {
		t.Fatal("unprofitable replacement bid admitted")
	}
}

// OP block 159027597: USDC -> OP on a dynamic-fee Slipstream pool charging 394
// pips (tick-spacing fee 10000, MEV tax from 0.3 gwei), then back to USDC at
// 500 pips, quoted 235,268 on 48,352,270. Charging the spacing fee as a bound
// turned the 0.49% edge negative and the route was never built; our undiscounted
// sender pays the live fee, which executor simulation confirmed at 394.
func caseFixture() (transactionView, arb.Opportunity) {
	usdc, op := domain.Address{1}, domain.Address{2}
	dynamic := domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{3}}
	back := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := transactionView{fundedView{
		dynamic: {Key: dynamic, Token0: usdc, Token1: op, Fee: 394, TickSpacing: 100, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: new(big.Int).Lsh(big.NewInt(1), 80),
			Slipstream: &pool.SlipstreamState{Valid: true, BaseFee: 150, BlockFee: 394, SpacingFee: 10000, FeeCap: 50000, TaxModule: domain.Address{5}, TaxMinimum: "300000000", TaxBaseFactor: "5", TaxMultiplier: 5_000_000}},
		back: {Key: back, Token0: usdc, Token1: op, Fee: 500, Reserve0: big.NewInt(1_005_763_000_000_000_000), Reserve1: big.NewInt(1e18)},
	}}
	return v, arb.Opportunity{Cycle: graph.Cycle{Start: usdc, Pools: []domain.PoolKey{dynamic, back}, Len: 2}, Input: big.NewInt(48_352_270)}
}

func TestBoundChargesTheLiveDynamicFeeNotTheSpacingFee(t *testing.T) {
	v, original := caseFixture()
	o, _, err := QuoteTransaction(t.Context(), v, original, big.NewInt(287), new(big.Int), 0)
	if err != nil || o.Net.Cmp(big.NewInt(230_000)) < 0 || o.Net.Cmp(big.NewInt(240_000)) > 0 {
		t.Fatal("fixture does not reproduce the quoted gross", o.Net, err)
	}
	tip := big.NewInt(10_000_000) // policy tip cap, below the 0.3 gwei tax threshold
	bounded, fees, err := QuoteTransactionBound(t.Context(), v, o, tip)
	if err != nil || fees[0].FeePips != 394 {
		t.Fatal("bound must charge the live fee", fees, err)
	}
	if bounded.Net.Cmp(o.Net) != 0 {
		t.Fatal("untaxed bound changed the quote", bounded.Net, o.Net)
	}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(287), Extra: big.NewInt(12_288_397_259), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 6, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1},
		GasLimit: 2_000_000, RouteGasBase: 250_000, RouteGasPerStep: 250_000, MaxFee: big.NewInt(20_000_000), MaxTip: tip, TargetTip: tip, MaxTotalFee: big.NewInt(10_000_000_000_000),
		MinimumNet: big.NewInt(1), BidShareBPS: 2000, SlippageBPS: 50, Lifetime: time.Second}
	version := domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{2}}
	// Native wei per 1e18 USDC units, as recorded on the reactive intent.
	usdc, _ := new(big.Int).SetString("444612399383677507402703043", 10)
	conversion := execution.FeeConversion{Numerator: big.NewInt(1e18), Denominator: usdc}
	intent, err := BuildReactive(t.Context(), v, o, nil, nil, version, conversion, costs, cfg, now)
	if err != nil {
		t.Fatal("the case's route was rejected:", err)
	}
	if intent.Call.TipCap.Cmp(tip) != 0 || intent.PoolFees[0].FeePips != 394 || intent.ExpectedNet.Cmp(big.NewInt(200_000)) < 0 {
		t.Fatal(intent.Call.TipCap, intent.PoolFees, intent.ExpectedNet)
	}
}

// Joint pricing must sign the same tip whose full pool-tax economics won.
// A post-selection halving ladder would invalidate its probability and EV.
func TestJointReactiveBidUsesRepricedSurplusAndExactSelectedQuote(t *testing.T) {
	v, o := transactionFixture()
	o, _, err := QuoteTransactionBound(t.Context(), v, o, new(big.Int))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 100000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(100000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 10000, SlippageBPS: 20, Lifetime: 500 * time.Millisecond}
	cfg.Bid = func(request BidRequest) *execution.BidDecision {
		if request.SurplusAtTip == nil {
			t.Fatal("pool tax absent from model")
		}
		low, ok := request.SurplusAtTip(1000)
		if !ok {
			t.Fatal("low bid rejected")
		}
		high, ok := request.SurplusAtTip(50000)
		if !ok || high.Cmp(low) >= 0 {
			t.Fatal("tax not deducted", low, high)
		}
		if _, ok = request.SurplusAtTip(100000); ok {
			t.Fatal("unfundable taxed bid admitted")
		}
		return &execution.BidDecision{ModelTip: big.NewInt(50000), Expected: big.NewInt(1), Applied: true, CostVersion: "joint-v1"}
	}
	version := domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{2}}
	costs := ReactiveCosts{BaseFee: big.NewInt(1), Extra: new(big.Int), ExpiresMS: now.Add(time.Second).UnixMilli()}
	intent, err := BuildReactive(t.Context(), v, o, nil, o.Input, version, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	quote, _, err := QuoteTransactionBound(t.Context(), v, o, big.NewInt(50000))
	if err != nil || intent.Call.TipCap.Int64() != 50000 || intent.QuotedGross.Cmp(quote.Net) != 0 {
		t.Fatal(intent, err)
	}
}
