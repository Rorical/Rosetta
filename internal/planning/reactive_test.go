package planning

import (
	"fmt"
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"math/big"
	"testing"
	"time"
)

type fundedView map[domain.PoolKey]pool.State

func (v fundedView) Pool(k domain.PoolKey) (pool.State, bool)   { p, ok := v[k]; return p, ok }
func (v fundedView) Deep(domain.PoolKey) (pool.DeepState, bool) { return pool.DeepState{}, false }
func TestReactiveFeeCapIncludesBidAndContractFeeGuard(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(108340), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, QuoteReserveBPS: 0, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	if i.Call.TipCap.Int64() != 108340 || i.Call.FeeCap.Int64() != 108928 {
		t.Fatal(i.Call)
	}
	if i.Route.MinProfit.Cmp(new(big.Int).Add(i.Budget, cfg.MinimumNet)) != 0 {
		t.Fatal("network cost missing from guard")
	}
	if err = i.Validate(t.Context(), now); err != nil {
		t.Fatal(err)
	}
}

// Robinhood's base fee sits at its 0.02 gwei floor, so a doubled cap charges
// the guard for headroom the chain never uses.
func TestReactiveBaseFeeHeadroomScalesCapBudgetAndGuard(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(400000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(401000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(1000), Extra: big.NewInt(0), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 4663}, Ordering: domain.OrderingArrival, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 300000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(0), TargetTip: big.NewInt(0), MaxTotalFee: big.NewInt(1000000000000), MinimumNet: big.NewInt(1), SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func(headroom uint64) (execution.ReactiveIntent, error) {
		cfg.BaseFeeHeadroomBPS = headroom
		return BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	}
	// 400M gross covers 1.25 x 1000 x 300k = 375M but not the doubled 600M.
	if _, err := build(0); err == nil {
		t.Fatal("doubled cap admitted a route below its guard")
	}
	i, err := build(12500)
	if err != nil {
		t.Fatal(err)
	}
	if i.Call.FeeCap.Int64() != 1250 || i.Budget.Int64() != 375000000 || i.Route.MinProfit.Int64() != 375000001 {
		t.Fatal("cap, budget or guard", i.Call.FeeCap, i.Budget, i.Route.MinProfit)
	}
	if err := i.Validate(t.Context(), now); err != nil {
		t.Fatal(err)
	}
}

// A rival can win at a tip above what the surplus allows per unit of gas
// limit (292M against 229M wei/gas). Reserving the tip on expected gas
// lets the share apply to the gas actually charged; the spend budget still
// covers the whole limit at the fee cap.
func TestReactiveReservesTipOnTipGas(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(2000000000), MaxTip: big.NewInt(1000000000), TargetTip: big.NewInt(1000000000), MaxTotalFee: big.NewInt(1000000000000000), MinimumNet: big.NewInt(1), BidShareBPS: 9000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func(cfg ReactivePolicy) execution.ReactiveIntent {
		i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		if err = i.Validate(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		return i
	}
	limit := build(cfg)
	cfg.TipGas = func(_ []domain.PoolKey, _ execution.Funding, limit uint64) uint64 { return limit / 2 }
	half := build(cfg)
	if half.TipGas != 250000 || limit.TipGas != 0 {
		t.Fatal("tip gas", half.TipGas, limit.TipGas)
	}
	// Half the gas doubles the tip per gas, and the base fee saved on the other
	// half adds to the surplus the share comes from.
	if doubled := new(big.Int).Mul(limit.Call.TipCap, big.NewInt(2)); half.Call.TipCap.Cmp(new(big.Int).Sub(doubled, big.NewInt(2))) < 0 {
		t.Fatal("share tip not priced on tip gas", half.Call.TipCap, limit.Call.TipCap)
	}
	if half.Budget.Cmp(new(big.Int).Mul(half.Call.FeeCap, big.NewInt(500000))) < 0 {
		t.Fatal("budget no longer covers the whole limit", half.Budget)
	}
	if half.Route.MinProfit.Cmp(new(big.Int).Add(half.GuardFee(), cfg.MinimumNet)) != 0 || half.GuardFee().Cmp(half.Budget) >= 0 {
		t.Fatal("profit guard", half.Route.MinProfit, half.GuardFee(), half.Budget)
	}
	tampered := half
	tampered.TipGas = 100000
	if tampered.Validate(t.Context(), now) == nil {
		t.Fatal("a changed tip gas passed validation")
	}
	tampered.TipGas = 500001
	if tampered.Validate(t.Context(), now) == nil {
		t.Fatal("tip gas above the limit passed validation")
	}
}

// Routes typically use about half their gas limit (two hops: 262k–397k of
// 757k). Charging the base fee on the whole limit would make the build reject
// routes whose gross covers the fees they pay, and the contract's profit guard
// demand them. Both charge the fee cap on the
// expected gas; the spend budget still covers the whole limit.
func TestReactiveChargesFeesOnExpectedGas(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(200000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(201000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(2000000000), MaxTip: big.NewInt(1000000000), TargetTip: big.NewInt(1000000000), MaxTotalFee: big.NewInt(1000000000000000), MinimumNet: big.NewInt(1), BidShareBPS: 9000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func(cfg ReactivePolicy) (execution.ReactiveIntent, error) {
		return BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	}
	// Twice the base fee on the 500k limit is 2.94e8, above the 2e8 gross.
	if _, err := build(cfg); err == nil {
		t.Fatal("the limit's base fee exceeds the gross; the build must refuse")
	}
	cfg.TipGas = func(_ []domain.PoolKey, _ execution.Funding, limit uint64) uint64 { return limit / 4 }
	i, err := build(cfg)
	if err != nil {
		t.Fatal("expected gas covers the fees, build refused:", err)
	}
	if err := i.Validate(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	guard := new(big.Int).Add(new(big.Int).Mul(i.Call.FeeCap, big.NewInt(125000)), costs.Extra)
	if i.TipGas != 125000 || i.GuardFee().Cmp(guard) != 0 || i.Route.MinProfit.Cmp(new(big.Int).Add(guard, cfg.MinimumNet)) != 0 {
		t.Fatal("profit guard not on expected gas", i.TipGas, i.GuardFee(), guard, i.Route.MinProfit)
	}
	if i.Budget.Cmp(new(big.Int).Mul(i.Call.FeeCap, big.NewInt(500000))) < 0 {
		t.Fatal("spend budget no longer covers the whole limit", i.Budget)
	}
}

func TestReactiveRouteGasAndDeadlineFollowPolicy(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 6, RouteGasBase: 250000, RouteGasPerStep: 250000, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 2000000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func(cfg ReactivePolicy) execution.ReactiveIntent {
		i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	routed := build(cfg)
	if routed.Call.GasLimit != 750000 || routed.Route.Deadline != uint64(now.Unix())+6 {
		t.Fatal("two-step route gas or deadline ignored", routed.Call.GasLimit, routed.Route.Deadline)
	}
	// A Slipstream swap hook needs gas left at the deepest nested swap.
	hooked := v[q]
	hooked.Slipstream = &pool.SlipstreamState{Hook: domain.Address{7}}
	v[q] = hooked
	if got := build(cfg).Call.GasLimit; got != 750000+slipstreamHookGas {
		t.Fatal("hooked route gas", got)
	}
	hooked.Slipstream = &pool.SlipstreamState{Hook: domain.Address{7}, Legacy: true}
	v[q] = hooked
	if got := build(cfg).Call.GasLimit; got != 750000 {
		t.Fatal("legacy fee module needs no hook gas", got)
	}
	// With SkipTaxedPools, a hook or an MEV tax module rejects the route; a
	// legacy fee module does not.
	skipping := cfg
	skipping.SkipTaxedPools = true
	if got := build(skipping).Call.GasLimit; got != 750000 {
		t.Fatal("legacy pool skipped", got)
	}
	for _, state := range []pool.SlipstreamState{{Hook: domain.Address{7}}, {TaxModule: domain.Address{7}}} {
		hooked.Slipstream = &state
		v[q] = hooked
		if _, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, skipping, now); err == nil || err.Error() != "taxed pool" {
			t.Fatalf("taxed pool %+v built: %v", state, err)
		}
	}
	v[q] = fundedView{q: {Key: q, Token0: a, Token1: b}}[q]
	cfg.RouteGasBase, cfg.RouteGasPerStep = 0, 0
	// A smaller reserved envelope leaves a larger expected net for the same quote.
	if global := build(cfg); global.Call.GasLimit != 2000000 || global.ExpectedNet.Cmp(routed.ExpectedNet) >= 0 {
		t.Fatal("route gas did not shrink the reserved budget", global.Call.GasLimit, global.ExpectedNet, routed.ExpectedNet)
	}
}

func TestReactiveSkipsQuarantinedLender(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	cheap, other := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{5}}, domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{6}}
	lenders := []pool.State{{Key: cheap, Token0: a, Token1: b, Fee: 100}, {Key: other, Token0: a, Token1: b, Fee: 500}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}, cheap: lenders[0], other: lenders[1]}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	cfg.PoolCheck = func(key domain.PoolKey) error {
		if key == cheap {
			return fmt.Errorf("quarantined")
		}
		return nil
	}
	i, err := BuildReactive(t.Context(), v, o, lenders, nil, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err != nil || i.Route.Funding != execution.FundingV3Flash || i.Route.Lender != other {
		t.Fatal("quarantined lender was chosen or blocked the route", err, i.Route.Lender)
	}
}

// A V3 flash lender must hold twice the principal of the token it lends: a
// lender holding less than the loan fails every preflight in its transfer.
// A lender whose
// balance is unknown is not used.
func TestReactiveSkipsLendersWithoutTheBalance(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	cheap, deep, unknown := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{5}}, domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{6}}, domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{7}}
	lenders := []pool.State{{Key: cheap, Token0: a, Token1: b, Fee: 100}, {Key: deep, Token0: a, Token1: b, Fee: 500}, {Key: unknown, Token0: a, Token1: b, Fee: 100}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}, cheap: lenders[0], deep: lenders[1], unknown: lenders[2]}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	balances := map[domain.PoolKey]*big.Int{cheap: big.NewInt(1999999), deep: big.NewInt(2000000)}
	cfg.LenderBalance = func(lender domain.PoolKey, token domain.Address) (*big.Int, bool) {
		if token != a {
			t.Fatal("asked for the balance of a token the route does not borrow", token)
		}
		balance, ok := balances[lender]
		return balance, ok
	}
	i, err := BuildReactive(t.Context(), v, o, lenders, nil, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err != nil || i.Route.Funding != execution.FundingV3Flash || i.Route.Lender != deep {
		t.Fatal("a lender without twice the principal, or of unknown balance, was chosen", err, i.Route.Lender)
	}
}

func TestReactiveBidModelSignsOnlyWhenApplied(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(20000000000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(20000000000000), AmountOut: big.NewInt(40000000000000)}, {Pool: q, AmountIn: big.NewInt(40000000000000), AmountOut: big.NewInt(20100000000000)}}}

	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(108340), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	expected := big.NewInt(1)
	build := func(applied bool) execution.ReactiveIntent {
		cfg.Bid = func(request BidRequest) *execution.BidDecision {
			surplus, gas, base, ceiling := request.Surplus, request.Gas, request.Base, request.Ceiling
			if request.Amount == nil || request.Amount.Cmp(o.Input) != 0 || request.Funding != execution.FundingWallet {
				t.Fatalf("bid model lost the route funding or input: %+v", request)
			}
			if surplus.Sign() <= 0 || gas != 500000 || base.Cmp(costs.BaseFee) != 0 {
				t.Fatal("bid model received the wrong inputs", surplus, gas, base)
			}
			// The surplus per unit of gas limit, 199,409, binds just below the
			// budget's (MaxTotalFee − extra)/gas − 2·base = 199,410.
			if ceiling.Int64() != 199_409 {
				t.Fatal("bid ceiling ignores the transaction budget", ceiling)
			}
			return &execution.BidDecision{ModelTip: big.NewInt(150000), Wins: 7, Samples: 10, Expected: expected, Applied: applied}
		}
		i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	if shadow := build(false); shadow.Call.TipCap.Int64() != 108340 || shadow.Bid == nil || shadow.Bid.ModelTip.Int64() != 150000 {
		t.Fatal("shadow model changed the signed tip or was not recorded", shadow.Call.TipCap, shadow.Bid)
	}
	if applied := build(true); applied.Call.TipCap.Int64() != 150000 || !applied.Bid.Applied {
		t.Fatal("applied model tip was not signed", applied.Call.TipCap)
	}
	// A route the model expects to lose money on is not sent when applied, and
	// is still recorded in shadow mode.
	expected = big.NewInt(-1)
	if shadow := build(false); shadow.Bid == nil || shadow.Bid.Expected.Sign() >= 0 {
		t.Fatal("shadow model blocked a send", shadow.Bid)
	}
	cfg.Bid = func(_ BidRequest) *execution.BidDecision {
		return &execution.BidDecision{ModelTip: new(big.Int), Expected: new(big.Int), Applied: true}
	}
	if _, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now); err == nil {
		t.Fatal("an applied bid with no expected profit was built")
	}
	// A decision that declines names its reason in the error the skip counter
	// keys on; a probe that breaks even is built.
	cfg.Bid = func(_ BidRequest) *execution.BidDecision {
		return &execution.BidDecision{ModelTip: big.NewInt(1000), Expected: big.NewInt(5), Applied: true, Reason: BidPredatory}
	}
	if _, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now); err == nil || err.Error() != "bid "+BidPredatory {
		t.Fatalf("declined bid: %v", err)
	}
	cfg.Bid = func(_ BidRequest) *execution.BidDecision {
		return &execution.BidDecision{ModelTip: new(big.Int), Expected: new(big.Int), Applied: true, Probe: true}
	}
	if probe, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now); err != nil || probe.Call.TipCap.Sign() != 0 {
		t.Fatalf("probe: %v", err)
	}
}

func TestRouteSizedFeeReserveStaysAboveTheFeeFunction(t *testing.T) {
	linear := LinearFee{X0: 100, X1: 2000, Y0: big.NewInt(1300), Y1: big.NewInt(7000)} // 1000 + 3x
	if got := linear.At(500); got.Int64() != 2500 {
		t.Fatalf("linear fee at 500 = %s, want 2500", got)
	}
	// A fee with a floor, max(2000, 3x), is convex: the chord stays above it.
	floored := LinearFee{X0: 100, X1: 2000, Y0: big.NewInt(2000), Y1: big.NewInt(6000)}
	for _, x := range []uint64{100, 500, 667, 1000, 1999, 2000} {
		if floored.At(x).Int64() < max(2000, 3*int64(x)) {
			t.Fatalf("chord below the fee at %d", x)
		}
	}
	worst := big.NewInt(1_000_000)
	costs := ReactiveCosts{Extra: worst}
	if costs.ExtraFor(2, 750_000) != worst {
		t.Fatal("unmeasured fees must reserve the worst case")
	}
	costs.L1 = &LinearFee{X0: 1000, X1: 5000, Y0: big.NewInt(10_000), Y1: big.NewInt(50_000)}
	costs.Operator = &LinearFee{X0: 100_000, X1: 2_000_000, Y0: new(big.Int), Y1: new(big.Int)}
	short, long := costs.ExtraFor(2, 750_000), costs.ExtraFor(6, 1_750_000)
	if short.Cmp(long) >= 0 || long.Cmp(worst) > 0 || short.Sign() <= 0 {
		t.Fatalf("route-sized reserves: short %s long %s worst %s", short, long, worst)
	}
}

// Nitro charges a transaction's L1 cost as poster gas inside its gas limit:
// a limit sized for execution alone fails the intrinsic gas check, and a
// budget of fee cap times that limit leaves the L1 cost out.
func TestReactiveNitroGasLimitCoversPosterGas(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100000001000000)}}}
	now := time.Now()
	base := big.NewInt(10_000_000) // Arbitrum's 0.01 gwei floor
	costs := ReactiveCosts{BaseFee: base, Extra: new(big.Int), ExpiresMS: now.Add(time.Second).UnixMilli(), Poster: &PosterPrice{PerTx: big.NewInt(1_000_000_000_000), PerByte: big.NewInt(2_000_000_000)}}
	cfg := ReactivePolicy{DeadlineSeconds: 6, RouteGasBase: 250000, RouteGasPerStep: 250000, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 2000000, MaxFee: big.NewInt(1_000_000_000), MaxTip: big.NewInt(0), TargetTip: big.NewInt(0), MaxTotalFee: big.NewInt(1_000_000_000_000_000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	size := int64(execution.PackedCalldataBound(2) + 200)
	cost := 2 * (1_000_000_000_000 + size*2_000_000_000)
	poster := uint64((cost + base.Int64() - 1) / base.Int64())
	if i.Call.GasLimit != 750000+poster {
		t.Fatalf("gas limit %d, want 750,000 execution + %d poster", i.Call.GasLimit, poster)
	}
	if want := new(big.Int).Mul(i.Call.FeeCap, new(big.Int).SetUint64(i.Call.GasLimit)); i.Budget.Cmp(want) != 0 {
		t.Fatalf("budget %s, want fee cap times the whole limit %s", i.Budget, want)
	}
	costs.Poster = nil
	if got := costs.PosterGas(2); got != 0 {
		t.Fatalf("poster gas %d without a price", got)
	}
}

// Under arrival ordering the share-of-surplus tip buys nothing: the intent
// signs the minimum tip, while priority ordering keeps the share.
func TestReactiveArrivalOrderingSignsTheMinimumTip(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: new(big.Int), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 6, RouteGasBase: 250000, RouteGasPerStep: 250000, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 4663}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 2000000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func(ordering domain.Ordering) *big.Int {
		cfg.Ordering = ordering
		i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		return i.Call.TipCap
	}
	if tip := build(domain.OrderingPriority); tip.Cmp(cfg.TargetTip) != 0 {
		t.Fatalf("priority ordering tip %s, want the target %s", tip, cfg.TargetTip)
	}
	if tip := build(domain.OrderingArrival); tip.Sign() != 0 {
		t.Fatalf("arrival ordering tip %s, want zero", tip)
	}
	cfg.MinimumTip = big.NewInt(7) // a replacement's required bump
	if tip := build(domain.OrderingArrival); tip.Int64() != 7 {
		t.Fatalf("arrival ordering tip %s, want the minimum 7", tip)
	}
}

// The bid model is told the base-fee reserve the surplus already excludes:
// the fee cap's base share on the gas the guard charges. OP's original
// policy reserved twice the base fee on the whole limit.
func TestReactiveBidRequestNamesTheReservedBaseFee(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b}, q: {Key: q, Token0: a, Token1: b}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	for _, tc := range []struct {
		headroom uint64
		tipGas   func([]domain.PoolKey, execution.Funding, uint64) uint64
		want     int64
	}{
		{want: 588 * 500000},
		{headroom: 11000, tipGas: func([]domain.PoolKey, execution.Funding, uint64) uint64 { return 200000 }, want: 324 * 200000},
	} {
		cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(108340), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond, BaseFeeHeadroomBPS: tc.headroom, TipGas: tc.tipGas}
		var reserved *big.Int
		cfg.Bid = func(request BidRequest) *execution.BidDecision {
			reserved = request.Reserved
			return nil
		}
		if _, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now); err != nil {
			t.Fatal(err)
		}
		if reserved == nil || reserved.Int64() != tc.want {
			t.Fatalf("headroom %d: reserved %v, want %d", tc.headroom, reserved, tc.want)
		}
	}
}

// A route V3 -> Camelot after a large buy on a long-tail Camelot pool: a
// rival sells into the Camelot pool ahead of the send. The V3 leg is held by
// a condition and unchanged; the Camelot leg has none, and its spot price
// still clears the cumulative precheck, so both swaps run before the profit
// check fails. A tip priced as if a loss cost only the condition check
// (about 11,000 gas) is then paid on the whole route (about 380,000 gas).
// A lost race is free only when every pool is held, and a
// wrong quote is ruled out only once the route has run as a whole. Otherwise
// the tip stays at the base fee while the route is worth little: a
// full revert then costs twice the base cost.
func TestTipStaysAtTheBaseFeeUnlessEveryPoolIsHeldAndTheRouteRan(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{p: {Key: p, Token0: a, Token1: b, Reserve0: big.NewInt(1e12), Reserve1: big.NewInt(2e12)}, q: {Key: q, Token0: a, Token1: b, Reserve0: big.NewInt(1e15), Reserve1: big.NewInt(2e10)}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli(), ConfigVersion: 5}
	slot := []execution.SlotCondition{{Slot: domain.Hash{8}, Value: domain.Hash{7}}}
	ceilingOf := func(held map[domain.PoolKey]bool, ran bool) int64 {
		o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
		cfg := ReactivePolicy{PackedConditions: true, PricePrecheck: true, CumulativePrecheck: true, DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond,
			QuietPool: func(state pool.State) []execution.SlotCondition {
				if held[state.Key] {
					return slot
				}
				return nil
			},
			RouteRan: func(funding execution.Funding, start domain.Address, pools []domain.PoolKey) bool {
				if funding != execution.FundingWallet || start != a || len(pools) != 2 {
					t.Fatalf("route identity lost: %v %v %v", funding, start, pools)
				}
				return ran
			}}
		var ceiling int64
		cfg.Bid = func(request BidRequest) *execution.BidDecision {
			ceiling = request.Ceiling.Int64()
			return &execution.BidDecision{ModelTip: new(big.Int).Set(request.Ceiling), Expected: big.NewInt(1), Applied: true}
		}
		i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		if i.Call.TipCap.Int64() != ceiling {
			t.Fatalf("signed tip %s, ceiling %d", i.Call.TipCap, ceiling)
		}
		return ceiling
	}
	both := map[domain.PoolKey]bool{p: true, q: true}
	if got := ceilingOf(both, true); got <= 294 {
		t.Fatalf("every pool held on a route that ran: capped at %d", got)
	}
	if got := ceilingOf(map[domain.PoolKey]bool{p: true}, true); got != 294 {
		t.Fatalf("one pool without a condition may tip %d, base fee 294", got)
	}
	// Every pool held but never run as a whole: a lost race is free, a wrong
	// quote is not. Capped at the base fee, under the rivals' typical median
	// tip of about one base fee, a route never wins and so never runs. Five
	// times the base fee clears that median.
	if got := ceilingOf(both, false); got != 5*294 {
		t.Fatalf("a held route that never ran may tip %d, want five base fees", got)
	}
	if got := ceilingOf(map[domain.PoolKey]bool{q: true}, false); got != 294 {
		t.Fatalf("an unheld pool on a route that never ran may tip %d, base fee 294", got)
	}
}
