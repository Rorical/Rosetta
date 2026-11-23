package planning

import (
	"bytes"
	"math/big"
	"reflect"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func hexInt(t *testing.T, text string) *big.Int {
	t.Helper()
	value, ok := new(big.Int).SetString(text, 0)
	if !ok {
		t.Fatalf("bad integer %q", text)
	}
	return value
}

func mustAddress(t *testing.T, text string) domain.Address {
	t.Helper()
	address, err := domain.ParseAddress(text)
	if err != nil {
		t.Fatal(err)
	}
	return address
}

// OP block 159035898 after a victim sold 0.05 WETH into Velodrome WETH/USDC,
// as read by ExecutorPriceCheckFork.t.sol (RouteTwoSuccessForkTest): Uniswap V3
// WETH/USDC 0.05% slot0 and the Velodrome reserves. The quoted profit of the
// 0.01 WETH route was 695195249487891 wei; the plan required half of it.
// Expected bounds are the ones the Solidity mirror encoded and the executor
// accepted on that state.
func TestPriceBoundsMatchForkedOptimismRoute(t *testing.T) {
	weth := mustAddress(t, "0x4200000000000000000000000000000000000006")
	usdc := mustAddress(t, "0x0b2C639c533813f4Aa9D7837CAf62653d097Ff85")
	uniswap := domain.PoolKey{Kind: domain.PoolKindV3, Address: mustAddress(t, "0x1fb3cf6e48F1E7B10213E7b6d87D4c073C7Fdb7b")}
	velodrome := domain.PoolKey{Kind: domain.PoolKindVelodromeV2, Address: mustAddress(t, "0xF4F2657AE744354bAcA871E56775e5083F7276Ab")}
	view := fundedView{
		uniswap:   {Key: uniswap, Token0: usdc, Token1: weth, SqrtPriceX96: hexInt(t, "0x523dc43695d11030c13e29f9c837")},
		velodrome: {Key: velodrome, Token0: usdc, Token1: weth, Reserve0: big.NewInt(2723049242), Reserve1: hexInt(t, "1306227000273787280"), Velodrome: &pool.VelodromeState{}},
	}
	plan := execution.ExecutorPlan{Asset: weth, Amount: hexInt(t, "10000000000000000"), MinProfit: hexInt(t, "347597624743945"), MaxLoanFee: new(big.Int),
		Steps: []execution.ExecutorStep{{Pool: uniswap, TokenIn: weth, TokenOut: usdc}, {Pool: velodrome, TokenIn: usdc, TokenOut: weth}}}
	output := new(big.Int).Add(plan.Amount, hexInt(t, "695195249487891"))
	if err := SetPriceBounds(view, &plan, output); err != nil {
		t.Fatal(err)
	}
	// The mirror rounds its square root in a different order; the ratio
	// bound uses the same operations and matches exactly.
	near := func(got, want *big.Int) bool {
		diff := new(big.Int).Sub(got, want)
		return new(big.Int).Mul(diff.Abs(diff), big.NewInt(1e12)).Cmp(want) <= 0
	}
	if mirror := hexInt(t, "0x539c773f367840f1478ef23d310b"); !near(plan.Steps[0].PriceBound, mirror) {
		t.Fatalf("V3 bound %x, mirror %x", plan.Steps[0].PriceBound, mirror)
	}
	if plan.Steps[1].PriceBound.Cmp(hexInt(t, "0x1ba9a4ad95c567dd8f614da9ad3443b9")) != 0 {
		t.Fatalf("Velodrome bound %x", plan.Steps[1].PriceBound)
	}
}

// accepts mirrors PriceCheck.within for a pool state and a bound.
func accepts(state pool.State, zeroForOne bool, bound *big.Int) bool {
	if state.Key.Kind == domain.PoolKindV2 || state.Key.Kind == domain.PoolKindVelodromeV2 {
		in, out := state.Reserve0, state.Reserve1
		if !zeroForOne {
			in, out = out, in
		}
		return new(big.Int).Lsh(out, 96).Cmp(new(big.Int).Mul(bound, in)) >= 0
	}
	if zeroForOne {
		return state.SqrtPriceX96.Cmp(bound) >= 0
	}
	return state.SqrtPriceX96.Cmp(bound) <= 0
}

func TestPriceBoundsAcceptTheQuoteAndRejectAMovePastTheSlack(t *testing.T) {
	required, output := big.NewInt(1_010_000), big.NewInt(1_030_000) // slack ≈ 1.94%
	// Rate after an adverse move of the slack plus or minus a hundredth of it.
	moved := func(state pool.State, zeroForOne bool, beyond bool) pool.State {
		num, den := new(big.Int).Mul(required, big.NewInt(100)), new(big.Int).Mul(output, big.NewInt(100))
		step := new(big.Int).Sub(output, required)
		if beyond {
			num.Sub(num, step)
		} else {
			num.Add(num, step)
		}
		// num/den is the remaining fraction of the leg's rate.
		next := state
		if state.Key.Kind == domain.PoolKindV2 || state.Key.Kind == domain.PoolKindVelodromeV2 {
			// Raise the input reserve so reserveOut/reserveIn falls to num/den of itself.
			if zeroForOne {
				next.Reserve0 = new(big.Int).Quo(new(big.Int).Mul(state.Reserve0, den), num)
			} else {
				next.Reserve1 = new(big.Int).Quo(new(big.Int).Mul(state.Reserve1, den), num)
			}
			return next
		}
		// The rate is price² for zeroForOne and 1/price² otherwise.
		squared := new(big.Int).Mul(state.SqrtPriceX96, state.SqrtPriceX96)
		if zeroForOne {
			squared.Mul(squared, num).Quo(squared, den)
		} else {
			squared.Mul(squared, den).Quo(squared, num)
		}
		next.SqrtPriceX96 = squared.Sqrt(squared)
		return next
	}
	a, b := domain.Address{1}, domain.Address{2}
	q96 := new(big.Int).Lsh(big.NewInt(1), 96)
	states := []pool.State{
		{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}, SqrtPriceX96: new(big.Int).Mul(q96, big.NewInt(3))},
		{Key: domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: domain.Address{4}}, SqrtPriceX96: new(big.Int).Quo(q96, big.NewInt(7))},
		{Key: domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{5}, PoolID: domain.Hash{1}}, SqrtPriceX96: q96},
		{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{6}}, Reserve0: big.NewInt(4e18), Reserve1: big.NewInt(9e9)},
		{Key: domain.PoolKey{Kind: domain.PoolKindVelodromeV2, Address: domain.Address{7}}, Reserve0: new(big.Int).Mul(big.NewInt(5e18), big.NewInt(100)), Reserve1: big.NewInt(3e18), Velodrome: &pool.VelodromeState{}},
	}
	for _, state := range states {
		for _, zeroForOne := range []bool{true, false} {
			state.Token0, state.Token1 = a, b
			tokenIn, tokenOut := a, b
			if !zeroForOne {
				tokenIn, tokenOut = b, a
			}
			plan := execution.ExecutorPlan{Asset: tokenIn, Amount: big.NewInt(1_000_000), MinProfit: big.NewInt(10_000), MaxLoanFee: new(big.Int),
				Steps: []execution.ExecutorStep{{Pool: state.Key, TokenIn: tokenIn, TokenOut: tokenOut}}}
			if err := SetPriceBounds(fundedView{state.Key: state}, &plan, output); err != nil {
				t.Fatal(err)
			}
			bound := plan.Steps[0].PriceBound
			if !accepts(state, zeroForOne, bound) || !accepts(moved(state, zeroForOne, false), zeroForOne, bound) {
				t.Fatalf("kind %d zeroForOne %v: quoted state or a move within the slack rejected", state.Key.Kind, zeroForOne)
			}
			if accepts(moved(state, zeroForOne, true), zeroForOne, bound) {
				t.Fatalf("kind %d zeroForOne %v: move past the slack accepted", state.Key.Kind, zeroForOne)
			}
		}
	}
}

func TestPriceBoundsSkipStablePairsAndNeedSlack(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	stable := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindVelodromeV2, Address: domain.Address{3}}, Token0: a, Token1: b,
		Reserve0: big.NewInt(1e18), Reserve1: big.NewInt(1e18), Velodrome: &pool.VelodromeState{Stable: true}}
	plan := execution.ExecutorPlan{Asset: a, Amount: big.NewInt(1000), MinProfit: big.NewInt(10), MaxLoanFee: big.NewInt(5),
		Steps: []execution.ExecutorStep{{Pool: stable.Key, TokenIn: a, TokenOut: b}}}
	view := fundedView{stable.Key: stable}
	if err := SetPriceBounds(view, &plan, big.NewInt(1100)); err != nil || plan.Steps[0].PriceBound != nil {
		t.Fatal("stable pair bounded", err)
	}
	// Principal, minimum profit and the loan-fee cap all count as required.
	if err := SetPriceBounds(view, &plan, big.NewInt(1015)); err == nil {
		t.Fatal("bounds set without slack")
	}
	plan.Steps[0].Pool.Address = domain.Address{9}
	if err := SetPriceBounds(view, &plan, big.NewInt(1100)); err == nil {
		t.Fatal("missing pool accepted")
	}
}

func TestReactivePricePrecheckFollowsPolicy(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{
		p: {Key: p, Token0: a, Token1: b, Reserve0: big.NewInt(1e12), Reserve1: big.NewInt(2e12)},
		q: {Key: q, Token0: a, Token1: b, Reserve0: big.NewInt(1e15), Reserve1: big.NewInt(2e10)},
	}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli()}
	cfg := ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func(cfg ReactivePolicy) execution.ReactiveIntent {
		i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := i.Validate(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		return i
	}
	// Byte 68 is the packed version after the selector and ABI head.
	if off := build(cfg); off.Call.Data[68] != 2 || off.Route.Steps[0].PriceBound != nil {
		t.Fatal("price bounds sent with the precheck off")
	}
	cfg.PricePrecheck = true
	on := build(cfg)
	if on.Call.Data[68] != 3 || on.Route.Steps[0].PriceBound == nil || on.Route.Steps[1].PriceBound == nil {
		t.Fatal("precheck did not bound every step")
	}
	cfg.CumulativePrecheck = true
	cumulative := build(cfg)
	if cumulative.Call.Data[68] != 4 || cumulative.Route.MinRate == nil || cumulative.Route.Steps[0].PriceBound == nil {
		t.Fatal("cumulative precheck did not set references and a rate")
	}
}

// cumulativeRate mirrors PriceCheck.rateChange and checkCumulative: the
// product, in 1e18, of each checked leg's rate now over its reference.
func cumulativeRate(t *testing.T, plan execution.ExecutorPlan, view fundedView) *big.Int {
	t.Helper()
	one, cap := big.NewInt(1e18), big.NewInt(4e18)
	product := new(big.Int).Set(one)
	for _, step := range plan.Steps {
		if step.PriceBound == nil {
			continue
		}
		state := view[step.Pool]
		zeroForOne := bytes.Compare(step.TokenIn[:], step.TokenOut[:]) < 0
		rate := new(big.Int)
		if state.Key.Kind == domain.PoolKindV2 || state.Key.Kind == domain.PoolKindVelodromeV2 {
			in, out := state.Reserve0, state.Reserve1
			if !zeroForOne {
				in, out = out, in
			}
			rate.Lsh(out, 96).Quo(rate, in).Mul(rate, one).Quo(rate, step.PriceBound)
		} else {
			if zeroForOne {
				rate.Mul(state.SqrtPriceX96, one).Quo(rate, step.PriceBound)
			} else {
				rate.Mul(step.PriceBound, one).Quo(rate, state.SqrtPriceX96)
			}
			if rate.Cmp(big.NewInt(2e18)) > 0 {
				rate.SetInt64(2e18)
			}
			rate.Mul(rate, rate).Quo(rate, one)
		}
		if rate.Cmp(cap) > 0 {
			rate.Set(cap)
		}
		product.Mul(product, rate).Quo(product, one)
		if product.Cmp(cap) > 0 {
			product.Set(cap)
		}
	}
	return product
}

// When every pool moves less than the slack, per-step bounds pass and the
// route fails at the closing profit check. With quote references the executor's
// product catches drift spread over legs and lets favourable moves offset.
func TestPriceReferencesCatchDriftSpreadOverLegs(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	q96 := new(big.Int).Lsh(big.NewInt(1), 96)
	v3 := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}, Token0: a, Token1: b, SqrtPriceX96: new(big.Int).Mul(q96, big.NewInt(3))}
	pair := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}, Token0: a, Token1: b, Reserve0: big.NewInt(1e18), Reserve1: big.NewInt(9e18)}
	view := fundedView{v3.Key: v3, pair.Key: pair}
	plan := execution.ExecutorPlan{Asset: a, Amount: big.NewInt(1_000_000), MinProfit: big.NewInt(1_000), MaxLoanFee: new(big.Int),
		Steps: []execution.ExecutorStep{{Pool: v3.Key, TokenIn: a, TokenOut: b}, {Pool: pair.Key, TokenIn: b, TokenOut: a}}}
	output := big.NewInt(1_003_000) // 2,000 of slack: about 20 bp
	if err := SetPriceReferences(view, &plan, output); err != nil {
		t.Fatal(err)
	}
	want := new(big.Int).Quo(new(big.Int).Mul(big.NewInt(1_001_000), big.NewInt(1e18)), output)
	if plan.Steps[0].PriceBound.Cmp(v3.SqrtPriceX96) != 0 || plan.MinRate.Cmp(want) != 0 {
		t.Fatal("references or rate", plan.Steps[0].PriceBound, plan.MinRate)
	}
	if cumulativeRate(t, plan, view).Cmp(big.NewInt(1e18)) != 0 {
		t.Fatal("an unchanged route is not exactly 1.0")
	}
	// Moves leg 0 (token0 sold, rate price²) and leg 1 (token1 sold for
	// token0, rate reserve0/reserve1) to these fractions of their rates.
	move := func(v3Bp, pairBp int64) fundedView {
		next := fundedView{}
		moved := v3
		root := new(big.Int).Sqrt(big.NewInt(v3Bp * 1e14))
		moved.SqrtPriceX96 = new(big.Int).Quo(new(big.Int).Mul(v3.SqrtPriceX96, root), big.NewInt(1e9))
		next[v3.Key] = moved
		shifted := pair
		shifted.Reserve1 = new(big.Int).Quo(new(big.Int).Mul(pair.Reserve1, big.NewInt(10000)), big.NewInt(pairBp))
		next[pair.Key] = shifted
		return next
	}
	if cumulativeRate(t, plan, move(9988, 9988)).Cmp(plan.MinRate) >= 0 {
		t.Fatal("12 bp on each leg, 24 bp together, accepted")
	}
	if cumulativeRate(t, plan, move(9975, 10010)).Cmp(plan.MinRate) < 0 {
		t.Fatal("25 bp against offset by 10 bp in favour rejected")
	}
}

func TestReactiveHotPathTakesV3FlashSwapRoutes(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{4}}
	price := new(big.Int).Lsh(big.NewInt(1), 96)
	v := fundedView{
		p: {Key: p, Token0: a, Token1: b, Fee: 500, SqrtPriceX96: price},
		q: {Key: q, Token0: a, Token1: b, Fee: 3000, SqrtPriceX96: new(big.Int).Rsh(price, 1)},
	}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli(), ConfigVersion: 5}
	cfg := ReactivePolicy{HotPath: true, PricePrecheck: true, CumulativePrecheck: true, DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func(costs ReactiveCosts) execution.ReactiveIntent {
		i, err := BuildReactive(t.Context(), v, o, nil, nil, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := i.Validate(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		return i
	}
	hot := build(costs)
	want := execution.HotConditions(cfg.Executor, 5, hot.Route.Deadline)
	if !hot.Route.Hot || hot.Route.MinRate != nil || hot.Route.Steps[0].PriceBound == nil || hot.Route.Steps[1].PriceBound == nil || !reflect.DeepEqual(hot.Conditions, want) {
		t.Fatalf("hot route %+v conditions %+v", hot.Route, hot.Conditions)
	}
	// A quiet pool is held by condition instead of its bound; the other leg
	// keeps its bound.
	held := []execution.SlotCondition{{Value: domain.Hash{7}}}
	cfg.QuietPool = func(state pool.State) []execution.SlotCondition {
		if state.Key == q {
			return held
		}
		return nil
	}
	quiet := build(costs)
	want.Accounts = append(want.Accounts, execution.AccountConditions{Address: q.Address, Slots: held})
	want.TimestampMax = quiet.Route.Deadline
	if quiet.Route.Steps[0].PriceBound == nil || quiet.Route.Steps[1].PriceBound != nil || !reflect.DeepEqual(quiet.Conditions, want) {
		t.Fatalf("quiet pool: route %+v conditions %+v", quiet.Route, quiet.Conditions)
	}
	cfg.QuietPool = nil
	// Without a verified executor version there is nothing to condition on.
	costs.ConfigVersion = 0
	if packed := build(costs); packed.Route.Hot || packed.Conditions != nil || packed.Call.Data[68] != 4 {
		t.Fatal("a hot route was built without conditions")
	}
}

// Packed routes keep their precheck and add the quiet pools' conditions.
func TestReactivePackedRouteHoldsQuietPools(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{
		p: {Key: p, Token0: a, Token1: b, Reserve0: big.NewInt(1e12), Reserve1: big.NewInt(2e12)},
		q: {Key: q, Token0: a, Token1: b, Reserve0: big.NewInt(1e15), Reserve1: big.NewInt(2e10)},
	}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli(), ConfigVersion: 5}
	held := []execution.SlotCondition{{Slot: domain.Hash{8}, Value: domain.Hash{7}}}
	cfg := ReactivePolicy{PackedConditions: true, PricePrecheck: true, CumulativePrecheck: true, QuietPool: func(state pool.State) []execution.SlotCondition {
		if state.Key == q {
			return held
		}
		return nil
	}, DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	i, err := BuildReactive(t.Context(), v, o, nil, o.Input, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	want := &execution.TransactionConditions{Accounts: []execution.AccountConditions{{Address: q.Address, Slots: held}}, TimestampMax: i.Route.Deadline}
	if i.Route.Hot || i.Call.Data[68] != 4 || i.Route.Steps[1].PriceBound == nil || !reflect.DeepEqual(i.Conditions, want) {
		t.Fatalf("packed route %+v conditions %+v", i.Route, i.Conditions)
	}
	if err := i.Validate(t.Context(), now); err != nil {
		t.Fatal(err)
	}
}

// With hot_chain the hot route is executeHot's chained form. It also
// takes a V2 pair, but only one a condition holds: the pair is sent its quoted
// amount and no price of it is read, so an unheld pair goes out packed, with
// its precheck.
func TestReactiveHotChainTakesAHeldPair(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	p, q := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}, domain.PoolKey{Kind: domain.PoolKindV2, Address: domain.Address{4}}
	v := fundedView{
		p: {Key: p, Token0: a, Token1: b, Fee: 500, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96)},
		q: {Key: q, Token0: a, Token1: b, Fee: 3000, Reserve0: big.NewInt(1e15), Reserve1: big.NewInt(2e10)},
	}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: a, Pools: []domain.PoolKey{p, q}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100000000000), Legs: []arb.Leg{{Pool: p, AmountIn: big.NewInt(1000000), AmountOut: big.NewInt(2000000)}, {Pool: q, AmountIn: big.NewInt(2000000), AmountOut: big.NewInt(100001000000)}}}
	now := time.Now()
	costs := ReactiveCosts{BaseFee: big.NewInt(294), Extra: big.NewInt(1000000), ExpiresMS: now.Add(time.Second).UnixMilli(), ConfigVersion: 5}
	held := []execution.SlotCondition{{Slot: domain.Hash{8}, Value: domain.Hash{7}}}
	holds := map[domain.PoolKey]bool{q: true}
	cfg := ReactivePolicy{HotPath: true, HotChain: true, PricePrecheck: true, CumulativePrecheck: true, QuietPool: func(state pool.State) []execution.SlotCondition {
		if holds[state.Key] {
			return held
		}
		return nil
	}, DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 500000, MaxFee: big.NewInt(1000000), MaxTip: big.NewInt(200000), TargetTip: big.NewInt(100000), MaxTotalFee: big.NewInt(100000000000), MinimumNet: big.NewInt(1), BidShareBPS: 8000, SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	build := func() execution.ReactiveIntent {
		i, err := BuildReactive(t.Context(), v, o, nil, nil, domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}, execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		if err := i.Validate(t.Context(), now); err != nil {
			t.Fatal(err)
		}
		return i
	}
	chained := build()
	want, err := execution.EncodeHotChainPlan(t.Context(), chained.Route)
	if err != nil {
		t.Fatal(err)
	}
	if !chained.Route.Hot || !chained.Route.Chain || chained.Route.Steps[0].PriceBound == nil || chained.Route.Steps[1].PriceBound != nil || !reflect.DeepEqual(chained.Call.Data, want) {
		t.Fatalf("chained route %+v", chained.Route)
	}
	if len(chained.Conditions.Accounts) != 2 || chained.Conditions.Accounts[1].Address != q.Address {
		t.Fatalf("the pair is not held: %+v", chained.Conditions)
	}
	// The pool is the outermost and carries its exact input; the pair after
	// it carries its quoted output.
	words := chained.Call.Data[68+53:]
	if new(big.Int).SetBytes(words[1:12]).Int64() != 1000000 || new(big.Int).SetBytes(words[33:44]).Int64() != 100001000000 {
		t.Fatalf("step amounts %x", words[:64])
	}
	holds[q] = false
	if packed := build(); packed.Route.Hot || packed.Route.Chain || packed.Route.MinRate == nil {
		t.Fatalf("an unheld pair went out on the chained path: %+v", packed.Route)
	}
}
