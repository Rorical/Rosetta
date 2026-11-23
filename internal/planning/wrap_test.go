package planning

import (
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// wrapRoute is WETH -> V3 -> USDT -> V4 (native ETH/USDT) -> ETH -> wrap ->
// WETH with its quote: 1e15 in, 2e9 USDT, then 1.1e15 ETH wrapped 1:1.
func wrapRoute() (fundedView, arb.Opportunity, [3]domain.PoolKey) {
	native, weth, usdt := domain.Address{}, domain.Address{0x82}, domain.Address{0xfd}
	v3 := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}
	v4 := domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{4}, PoolID: domain.Hash{1}}
	wrap := domain.PoolKey{Kind: domain.PoolKindWrap, Address: weth}
	price := new(big.Int).Lsh(big.NewInt(1), 96)
	view := fundedView{
		v3:   {Key: v3, Token0: weth, Token1: usdt, Fee: 500, TickSpacing: 10, SqrtPriceX96: price, Liquidity: big.NewInt(1e18)},
		v4:   {Key: v4, Token0: native, Token1: usdt, Fee: 500, TickSpacing: 10, SqrtPriceX96: price, Liquidity: big.NewInt(1e18)},
		wrap: {Key: wrap, Token0: native, Token1: weth},
	}
	in, middle, out := big.NewInt(1e15), big.NewInt(2e9), big.NewInt(11e14)
	o := arb.Opportunity{
		Cycle: graph.Cycle{Start: weth, Pools: []domain.PoolKey{v3, v4, wrap}, Len: 3}, Input: in, Net: big.NewInt(1e14),
		Legs: []arb.Leg{{Pool: v3, AmountIn: in, AmountOut: middle}, {Pool: v4, AmountIn: middle, AmountOut: out}, {Pool: wrap, AmountIn: out, AmountOut: out}},
	}
	return view, o, [3]domain.PoolKey{v3, v4, wrap}
}

func wrapPolicy() ReactivePolicy {
	return ReactivePolicy{WrapNative: true, DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1},
		GasLimit: 800000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100), MaxTotalFee: big.NewInt(1e12), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
}

func buildWrap(t *testing.T, view fundedView, o arb.Opportunity, cfg ReactivePolicy, costs ReactiveCosts) (execution.ReactiveIntent, error) {
	t.Helper()
	now := time.Now()
	if costs.BaseFee == nil {
		costs.BaseFee, costs.Extra = big.NewInt(294), big.NewInt(1000000)
	}
	costs.ExpiresMS = now.Add(time.Second).UnixMilli()
	version := domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}
	return BuildReactive(t.Context(), view, o, nil, o.Input, version, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
}

// The route ends with a WRAP step on the wrapped token: native currency in,
// the token out, no fee, and the leg's amounts under the route's slippage
// like every other leg. The packed call is what the executor decodes.
func TestReactiveRouteWrapsANativeV4Output(t *testing.T) {
	view, o, keys := wrapRoute()
	weth := o.Cycle.Start
	intent, err := buildWrap(t, view, o, wrapPolicy(), ReactiveCosts{})
	if err != nil {
		t.Fatal(err)
	}
	route := intent.Route
	if route.Funding != execution.FundingV3FlashSwap || route.Asset != weth || len(route.Steps) != 3 || route.Hot {
		t.Fatalf("route %+v", route)
	}
	step := route.Steps[2]
	if step.Pool != keys[2] || step.Pool.Address != weth || step.TokenIn != (domain.Address{}) || step.TokenOut != weth || step.Fee != 0 || step.TickSpacing != 0 {
		t.Fatalf("wrap step %+v", step)
	}
	if step.AmountIn.Cmp(step.AmountOut) != 0 || step.AmountOut.Int64() != 11e14 || step.SqrtPriceLimitX96 != nil || step.PriceBound != nil {
		t.Fatalf("wrap amounts %+v", step)
	}
	// 50 bps under the quote, as the V4 leg that feeds it.
	if want := int64(11e14 * 9950 / 10000); step.MinAmountOut.Int64() != want || route.Steps[1].MinAmountOut.Int64() != want {
		t.Fatalf("minimums %s and %s, want %d", route.Steps[1].MinAmountOut, step.MinAmountOut, want)
	}
	packed, err := execution.EncodePackedExecutorPlan(t.Context(), route)
	if err != nil {
		t.Fatal(err)
	}
	if string(packed) != string(intent.Call.Data) {
		t.Fatal("the intent's call is not the packed route")
	}
	last := packed[68+95+160 : 68+95+240]
	if last[0] != 9 || domain.Address(last[1:21]) != weth || domain.Address(last[21:41]) != weth {
		t.Fatalf("packed wrap step %x", last)
	}

	// The same loop entered at the withdrawal has no flash swap to start on.
	turned := o
	turned.Cycle = graph.Cycle{Start: weth, Pools: []domain.PoolKey{keys[2], keys[1], keys[0]}, Len: 3}
	turned.Legs = []arb.Leg{{Pool: keys[2], AmountIn: o.Input, AmountOut: o.Input}, {Pool: keys[1], AmountIn: o.Input, AmountOut: big.NewInt(2e9)}, {Pool: keys[0], AmountIn: big.NewInt(2e9), AmountOut: big.NewInt(11e14)}}
	for _, choice := range FundingChoices(turned, nil, nil, nil) {
		t.Fatalf("a route starting on the wrap is funded by %v without a wallet, inventory or lender", choice.Funding)
	}
	intent, err = buildWrap(t, view, turned, wrapPolicy(), ReactiveCosts{})
	if err != nil {
		t.Fatal(err)
	}
	if intent.Route.Funding != execution.FundingWallet || intent.Route.Steps[0].Pool != keys[2] || intent.Route.Steps[0].TokenOut != (domain.Address{}) || intent.Route.Steps[0].MinAmountOut.Int64() != 1e15*9950/10000 {
		t.Fatalf("withdrawal route %+v", intent.Route)
	}
}

// The deployed executor may predate the WRAP leg: without wrap_native no
// route through the wrap pool is built, whatever the view holds.
func TestReactiveRefusesAWrapLegUnlessEnabled(t *testing.T) {
	view, o, _ := wrapRoute()
	cfg := wrapPolicy()
	cfg.WrapNative = false
	if _, err := buildWrap(t, view, o, cfg, ReactiveCosts{}); err == nil || !strings.Contains(err.Error(), "wrap leg not enabled") {
		t.Fatalf("built a wrap route with the gate off: %v", err)
	}
}

// The wrap has no state a rival can move, so it counts as held without a
// condition: the tip bound, the loss gas and the conditions all depend on
// the other pools alone. The precheck leaves the leg unchecked.
func TestWrapLegCountsAsHeldWithoutACondition(t *testing.T) {
	view, o, keys := wrapRoute()
	slot := []execution.SlotCondition{{Slot: domain.Hash{8}, Value: domain.Hash{7}}}
	held := map[domain.PoolKey]bool{}
	cfg := wrapPolicy()
	cfg.PackedConditions, cfg.PricePrecheck, cfg.CumulativePrecheck = true, true, true
	cfg.QuietPool = func(state pool.State) []execution.SlotCondition {
		if state.Key.Kind == domain.PoolKindWrap {
			t.Fatal("asked for conditions on the wrap pool")
		}
		if held[state.Key] {
			return slot
		}
		return nil
	}
	cfg.RouteRan = func(execution.Funding, domain.Address, []domain.PoolKey) bool { return true }
	var request BidRequest
	cfg.Bid = func(r BidRequest) *execution.BidDecision {
		request = r
		return &execution.BidDecision{ModelTip: new(big.Int).Set(r.Ceiling), Expected: big.NewInt(1), Applied: true}
	}
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ConfigVersion: 5}

	held[keys[0]] = true
	if _, err := buildWrap(t, view, o, cfg, costs); err != nil {
		t.Fatal(err)
	}
	// The route ran and is worth more than valueShareSurplus: it bids its
	// share of the value (here the configured maximum), not the base fee.
	if request.Ceiling.Int64() <= 294 || request.ShareTip == nil || request.ShareTip.Cmp(request.Ceiling) != 0 || request.LossGas == FreeLoss {
		t.Fatalf("an unheld V4 pool: ceiling %s, share tip %v, loss gas %d", request.Ceiling, request.ShareTip, request.LossGas)
	}
	held[keys[1]] = true
	intent, err := buildWrap(t, view, o, cfg, costs)
	if err != nil {
		t.Fatal(err)
	}
	if request.Ceiling.Int64() <= 294 || request.LossGas != FreeLoss || !request.Conditional {
		t.Fatalf("both swap pools held: ceiling %s, loss gas %d", request.Ceiling, request.LossGas)
	}
	if intent.Conditions == nil || len(intent.Conditions.Accounts) != 2 {
		t.Fatalf("conditions %+v", intent.Conditions)
	}
	for _, account := range intent.Conditions.Accounts {
		if account.Address == keys[2].Address {
			t.Fatal("a condition holds the wrapped token")
		}
	}
	steps := intent.Route.Steps
	if intent.Route.MinRate == nil || steps[0].PriceBound == nil || steps[1].PriceBound == nil || steps[2].PriceBound != nil {
		t.Fatalf("precheck references %v %v %v", steps[0].PriceBound, steps[1].PriceBound, steps[2].PriceBound)
	}
	cfg.CumulativePrecheck = false
	if intent, err = buildWrap(t, view, o, cfg, costs); err != nil || intent.Route.Steps[2].PriceBound != nil || intent.Route.Steps[1].PriceBound == nil {
		t.Fatalf("per-step bounds: %v", err)
	}
}

// A hot route never carries a wrap leg, chained or not.
func TestWrapRouteIsNeverHot(t *testing.T) {
	view, o, _ := wrapRoute()
	for _, chain := range []bool{false, true} {
		cfg := wrapPolicy()
		cfg.HotPath, cfg.HotChain = true, chain
		cfg.QuietPool = func(pool.State) []execution.SlotCondition { return []execution.SlotCondition{{}} }
		intent, err := buildWrap(t, view, o, cfg, ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ConfigVersion: 5})
		if err != nil {
			t.Fatal(err)
		}
		if intent.Route.Hot || intent.Conditions != nil {
			t.Fatalf("chain %v: hot %v, conditions %+v", chain, intent.Route.Hot, intent.Conditions)
		}
	}
}
