package planning

import (
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// metricRoute is the reserve fixture's route with its second pool turned
// into a MetricOmm pool whose quote is age old.
func metricRoute(now time.Time, age time.Duration, mutate func(*pool.MetricState)) (fundedView, domain.PoolKey) {
	view, o, _, _, _, _ := reactiveReserveFixture()
	key := o.Cycle.Pools[1]
	state := view[key]
	state.Metric = &pool.MetricState{BidX64: big.NewInt(1), AskX64: big.NewInt(2), QuoteMS: uint64(now.Add(-age).UnixMilli())}
	if mutate != nil {
		mutate(state.Metric)
	}
	view[key] = state
	return view, key
}

// The pool's price provider reverts outside −1,000 ms < block time − quote
// time ≤ 10,000 ms. A route is built only on a quote at most 5 s old, and
// never on a stale pool or one without a quote.
func TestReactiveBuildRefusesOldMetricQuotes(t *testing.T) {
	_, o, costs, cfg, version, now := reactiveReserveFixture()
	conversion := execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}
	// An executor without the leg would revert on chain: off unless enabled.
	fresh, _ := metricRoute(now, 300*time.Millisecond, nil)
	if _, err := BuildReactive(t.Context(), fresh, o, nil, o.Input, version, conversion, costs, cfg, now); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("a MetricOmm route was built without metric_omm: %v", err)
	}
	cfg.MetricOmm = true
	for _, c := range []struct {
		name   string
		age    time.Duration
		mutate func(*pool.MetricState)
		refuse string
	}{
		{name: "fresh", age: 300 * time.Millisecond},
		{name: "two updates old", age: 4900 * time.Millisecond},
		{name: "stamped just ahead", age: -900 * time.Millisecond},
		{name: "too old", age: 5100 * time.Millisecond, refuse: "age window"},
		{name: "past the chain's window", age: 11 * time.Second, refuse: "age window"},
		{name: "stamped a second ahead", age: -time.Second, refuse: "age window"},
		{name: "stale pool", age: 300 * time.Millisecond, mutate: func(m *pool.MetricState) { m.Stale = true }, refuse: "unavailable"},
		{name: "no quote", age: 300 * time.Millisecond, mutate: func(m *pool.MetricState) { m.BidX64, m.AskX64 = nil, nil }, refuse: "unavailable"},
	} {
		view, _ := metricRoute(now, c.age, c.mutate)
		_, err := BuildReactive(t.Context(), view, o, nil, o.Input, version, conversion, costs, cfg, now)
		if c.refuse == "" && err != nil {
			t.Errorf("%s: refused: %v", c.name, err)
		}
		if c.refuse != "" && (err == nil || !strings.Contains(err.Error(), c.refuse)) {
			t.Errorf("%s: built or refused for another reason: %v", c.name, err)
		}
	}
}

// The executor's price check reads no MetricOmm pool, so neither precheck
// gives the leg a price, while the other leg keeps its own.
func TestPricePrecheckLeavesMetricLegUnbound(t *testing.T) {
	_, o, costs, cfg, version, now := reactiveReserveFixture()
	conversion := execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}
	for _, cumulative := range []bool{false, true} {
		view, key := metricRoute(now, 0, nil)
		first := view[o.Cycle.Pools[0]]
		first.Reserve0, first.Reserve1 = big.NewInt(1_000_000_000_000), big.NewInt(1_000_000_000_000)
		view[first.Key] = first
		metric := view[key]
		metric.Key.Kind = domain.PoolKindMetricOmm
		delete(view, key)
		view[metric.Key] = metric
		route := o
		route.Cycle.Pools = []domain.PoolKey{first.Key, metric.Key}
		route.Legs[1].Pool = metric.Key
		cfg.PricePrecheck, cfg.CumulativePrecheck, cfg.MetricOmm = true, cumulative, true
		intent, err := BuildReactive(t.Context(), view, route, nil, o.Input, version, conversion, costs, cfg, now)
		if err != nil {
			t.Fatal(err)
		}
		steps := intent.Route.Steps
		if steps[0].PriceBound == nil || steps[1].PriceBound != nil || steps[1].SqrtPriceLimitX96 != nil || (intent.Route.MinRate != nil) != cumulative {
			t.Fatalf("cumulative %v: bounds %v, %v", cumulative, steps[0].PriceBound, steps[1].PriceBound)
		}
		if _, err := execution.EncodePackedExecutorPlan(t.Context(), intent.Route); err != nil {
			t.Fatal(err)
		}
	}
}

// A route ending in a MetricOmm pool runs to its end and reverts on the
// profit check if another swap moved the pool first, costing several times
// the route's expected net: the pool's leg has no price precheck. The pool's
// slot 0
// (bin and position) is known exactly, so the route is held at it and a moved
// pool voids the send for free.
//
// The oracle's word is not held: the oracle is updated about every 2 s and
// the sequencer admits a condition only if it also held 2 s earlier, so most
// sends holding the oracle word would be rejected. A newer quote ahead of the
// send is therefore paid for, and the leg does not count as held when the loss and
// the tip are priced.
func TestMetricLegIsHeldAtThePoolPositionOnly(t *testing.T) {
	key := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: domain.Address{0x2b}}
	other := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{3}}
	metric := &pool.MetricState{Words: []domain.Hash{{1, 2, 3}, {}, {}}, Oracle: domain.Address{0xe8}, OracleSlot: domain.Hash{0x8d}, OracleWord: domain.Hash{5, 6}}
	view := fundedView{key: {Key: key, Metric: metric}, other: {Key: other}}
	route := execution.ExecutorPlan{Steps: []execution.ExecutorStep{{Pool: other}, {Pool: key}}}
	held := &execution.TransactionConditions{}
	holdQuietPools(view, &route, held, func(pool.State) []execution.SlotCondition { return nil }, false)
	want := []execution.AccountConditions{{Address: key.Address, Slots: []execution.SlotCondition{{Slot: domain.Hash{}, Value: metric.Words[0]}}}}
	if !reflect.DeepEqual(held.Accounts, want) {
		t.Fatalf("conditions %+v, want %+v", held.Accounts, want)
	}
	// With the other pool held, the route is still not free to lose and
	// tips no more than one base fee.
	quiet := func(state pool.State) []execution.SlotCondition {
		if state.Key == other {
			return []execution.SlotCondition{{}}
		}
		return nil
	}
	o := arb.Opportunity{Cycle: graph.Cycle{Pools: []domain.PoolKey{other, key}, Len: 2}}
	cfg := ReactivePolicy{PackedConditions: true, QuietPool: quiet}
	if gas, conditional := lossGas(cfg, view, o, execution.FundingWallet, ReactiveCosts{}); gas == FreeLoss || !conditional {
		t.Fatalf("loss gas %d, conditional %v", gas, conditional)
	}
	if fees, byValue := cautiousTip(cfg, view, o, execution.FundingWallet); fees != 1 || byValue {
		t.Fatalf("tip bound of %d base fees, by value %v", fees, byValue)
	}
}

// Large routes rarely win under a tip cap in base fees: rivals on the same
// route typically tip about half of the worth, and a few base fees lose the
// sum of reverts. A route with an unheld pool behind a price bound keeps the one
// base fee cap until it is worth valueShareSurplus, then bids half its worth,
// and only once it has run as a whole. A MetricOmm leg or a packed route
// without the precheck never does.
func TestAnUnheldPoolBehindAPriceBoundBidsHalfTheWorthOfALargeRoute(t *testing.T) {
	trigger := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{1}}
	resting := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{2}}
	view := fundedView{trigger: {Key: trigger}, resting: {Key: resting}}
	quiet := func(state pool.State) []execution.SlotCondition {
		if state.Key == resting {
			return []execution.SlotCondition{{}}
		}
		return nil
	}
	o := arb.Opportunity{Cycle: graph.Cycle{Pools: []domain.PoolKey{trigger, resting}, Len: 2}}
	ran := true
	cfg := ReactivePolicy{PricePrecheck: true, QuietPool: quiet, RouteRan: func(execution.Funding, domain.Address, []domain.PoolKey) bool { return ran }}
	if fees, byValue := cautiousTip(cfg, view, o, execution.FundingWallet); fees != 1 || !byValue {
		t.Fatalf("tip bound of %d base fees, by value %v", fees, byValue)
	}
	ran = false
	if fees, byValue := cautiousTip(cfg, view, o, execution.FundingWallet); fees != 1 || byValue {
		t.Fatalf("a route that never ran: %d base fees, by value %v", fees, byValue)
	}
	ran, cfg.PricePrecheck = true, false
	if fees, byValue := cautiousTip(cfg, view, o, execution.FundingWallet); fees != 1 || byValue {
		t.Fatalf("no price bound: %d base fees, by value %v", fees, byValue)
	}
	if tip := valueShareTip(big.NewInt(39_999_999_999_999), 400_000); tip != nil {
		t.Fatalf("a small route bids %s by value", tip)
	}
	// Routes worth about $0.10 after fees lose to rivals at 7 to 21 base
	// fees if held at one base fee, so the threshold sits below them. $0.10 at
	// 444,093 gas bids 2.9 base fees of 2e7.
	if tip := valueShareTip(big.NewInt(51_000_000_000_000), 444_093); tip == nil || tip.Int64() != 57_420_405 {
		t.Fatalf("a $0.10 route bids %v by value", tip)
	}
	// $0.69 at 475,667 gas: half is 3.5e8 a gas, well above a rival's
	// typical 0.9e8.
	if tip := valueShareTip(big.NewInt(330_000_000_000_000), 475_667); tip == nil || tip.Int64() != 346_881_326 {
		t.Fatalf("value share tip %v", tip)
	}
}
