package arb

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestNewValidation(t *testing.T) {
	table := mustTable(t, 2, testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000))
	notional := fixedNotional(1_000)
	quoter := identityQuoter()
	tests := []struct {
		name     string
		table    CycleTable
		notional NotionalFunc
		k        int
		quoter   Quoter
		wantErr  error
	}{
		{name: "nil table", table: nil, notional: notional, k: 1, quoter: quoter, wantErr: ErrInvalidConfig},
		{name: "nil notional", table: table, notional: nil, k: 1, quoter: quoter, wantErr: ErrInvalidConfig},
		{name: "zero k", table: table, notional: notional, k: 0, quoter: quoter, wantErr: ErrInvalidConfig},
		{name: "negative k", table: table, notional: notional, k: -1, quoter: quoter, wantErr: ErrInvalidConfig},
		{name: "nil quoter", table: table, notional: notional, k: 1, quoter: nil, wantErr: ErrInvalidConfig},
		{name: "valid", table: table, notional: notional, k: 1, quoter: quoter},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := New(test.table, test.notional, test.k, test.quoter)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				if engine != nil {
					t.Fatal("engine = non-nil, want nil on error")
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if engine == nil {
				t.Fatal("engine = nil, want non-nil")
			}
		})
	}
}

func TestEvaluateProfitableCycle(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA, poolB)
	quoter := v2CurveQuoter()
	engine := mustEngine(t, table, 1_000, 1, quoter)
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}
	opportunity := opportunities[0]
	if opportunity.Cycle.Start != testAddress(0x01) {
		t.Fatalf("Start = %s, want %s", opportunity.Cycle.Start, testAddress(0x01))
	}
	// Both pools price token0 at 2 token1, so the cycle pays 4x at the margin.
	// The closed form replaces the 1_000 notional with a much larger input.
	requireBig(t, opportunity.Input, 333_333, "Input")
	requireBig(t, opportunity.Output, 666_665, "Output")
	requireBig(t, opportunity.Net, 333_332, "Net")
	if notionalNet := quoteCycleNet(t, poolA, poolB, 1_000); opportunity.Net.Cmp(notionalNet) <= 0 {
		t.Fatalf("Net = %s, want more than the notional quote %s", opportunity.Net, notionalNet)
	}
	if len(opportunity.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(opportunity.Legs))
	}
	if opportunity.Legs[0].Pool != poolA.Key || opportunity.Legs[1].Pool != poolB.Key {
		t.Fatalf("leg pools = %+v, want [%+v %+v]", opportunity.Legs, poolA.Key, poolB.Key)
	}
	requireBig(t, opportunity.Legs[0].AmountIn, 333_333, "leg 0 in")
	requireBig(t, opportunity.Legs[0].AmountOut, 499_999, "leg 0 out")
	requireBig(t, opportunity.Legs[1].AmountIn, 499_999, "leg 1 in")
	requireBig(t, opportunity.Legs[1].AmountOut, 666_665, "leg 1 out")

	wantCalls := []quoteCall{
		{pool: poolA.Key.Address, token: poolA.Token0, amount: big.NewInt(1_000)},
		{pool: poolB.Key.Address, token: poolB.Token0, amount: big.NewInt(1_998)},
		{pool: poolA.Key.Address, token: poolA.Token0, amount: big.NewInt(333_333)},
		{pool: poolB.Key.Address, token: poolB.Token0, amount: big.NewInt(499_999)},
	}
	if len(quoter.calls) != len(wantCalls) {
		t.Fatalf("quote calls = %d, want %d", len(quoter.calls), len(wantCalls))
	}
	for i, want := range wantCalls {
		got := quoter.calls[i]
		if got.pool != want.pool || got.token != want.token || got.amount.Cmp(want.amount) != 0 {
			t.Fatalf("call %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestEvaluateUnprofitableCycleReturned(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 2_000_000, 1_000_000)
	table := mustTable(t, 2, poolA, poolB)
	engine := mustEngine(t, table, 1_000, 10, constantQuoter(998))
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{
		poolA.Key,
		poolB.Key,
	})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 2 {
		t.Fatalf("opportunities = %d, want 2", len(opportunities))
	}
	for i, opportunity := range opportunities {
		if opportunity.Net.Sign() >= 0 {
			t.Fatalf("opportunity %d Net = %s, want negative", i, opportunity.Net)
		}
		requireBig(t, opportunity.Output, 998, fmt.Sprintf("opportunity %d Output", i))
	}
}

func TestEvaluateEmptyDirty(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 10, identityQuoter())
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(context.Background(), view, nil)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if opportunities != nil {
		t.Fatalf("opportunities = %v, want nil", opportunities)
	}
}

func TestEvaluateMissingPoolSkipsCycle(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	quoter := identityQuoter()
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 10, quoter)
	view := newFakeView().add(poolA)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 0 {
		t.Fatalf("opportunities = %d, want 0", len(opportunities))
	}
	if len(quoter.calls) != 1 {
		t.Fatalf("quote calls = %d, want 1 before the missing pool is reached", len(quoter.calls))
	}
}

func TestNeedsDeep(t *testing.T) {
	tests := []struct {
		kind domain.PoolKind
		want bool
	}{
		{domain.PoolKindV2, false},
		{domain.PoolKindV3, true},
		{domain.PoolKindV4, true},
		{domain.PoolKindCamelotV3, true},
		{domain.PoolKindSlipstream, true},
	}
	for _, test := range tests {
		t.Run(fmt.Sprint(test.kind), func(t *testing.T) {
			if got := needsDeep(test.kind); got != test.want {
				t.Fatalf("needsDeep(%d) = %t, want %t", test.kind, got, test.want)
			}
		})
	}
}

func TestEvaluateV3ShapedWithoutDeepSkipsCycle(t *testing.T) {
	kinds := []domain.PoolKind{
		domain.PoolKindV3,
		domain.PoolKindCamelotV3,
		domain.PoolKindSlipstream,
	}
	for _, kind := range kinds {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			poolA := testV3State(0x10, 0x01, 0x02, 3000, 10, 1_000_000_000)
			poolA.Key.Kind = kind
			poolB := testV3State(0x20, 0x02, 0x01, 3000, 10, 1_000_000_000)
			poolB.Key.Kind = kind
			engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000_000, 1, failQuoter(t))
			view := newFakeView().add(poolA).add(poolB)

			opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(opportunities) != 0 {
				t.Fatalf("opportunities = %d, want 0", len(opportunities))
			}
		})
	}
}

func TestEvaluatePassesDeepStateForV3(t *testing.T) {
	poolA := testV3State(0x10, 0x01, 0x02, 3000, 10, 1_000_000_000)
	poolB := testV3State(0x20, 0x02, 0x01, 3000, 10, 1_000_000_000)
	deepA := emptyDeep()
	deepA.Ticks[42] = pool.TickInfo{
		LiquidityGross: big.NewInt(7),
		LiquidityNet:   big.NewInt(-3),
	}
	view := newFakeView().
		add(poolA).add(poolB).
		addDeep(poolA.Key, deepA).
		addDeep(poolB.Key, emptyDeep())
	quoter := &fakeQuoter{quote: func(state pool.State, deep pool.DeepState, _ domain.Address, amountIn *big.Int) (*big.Int, error) {
		if state.Key.Address == poolA.Key.Address {
			info, ok := deep.Ticks[42]
			if !ok || info.LiquidityGross == nil || info.LiquidityGross.Cmp(big.NewInt(7)) != 0 {
				t.Fatalf("deep state for %s = %+v, want tick 42 liquidity 7", state.Key.Address, deep)
			}
		}
		return new(big.Int).Set(amountIn), nil
	}}
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 1, quoter)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}
}

func TestEvaluateKCapsCandidates(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x01, 0x02, 1_000_000, 2_000_000)
	poolC := testV2State(0x30, 0x01, 0x02, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA, poolB, poolC)
	view := newFakeView().add(poolA).add(poolB).add(poolC)
	dirty := []domain.PoolKey{poolA.Key}

	tests := []struct {
		k    int
		want int
	}{
		{k: 1, want: 1},
		{k: 2, want: 2},
		{k: 10, want: 4},
	}
	for _, test := range tests {
		t.Run(fmt.Sprintf("k=%d", test.k), func(t *testing.T) {
			engine := mustEngine(t, table, 1_000, test.k, identityQuoter())
			opportunities, err := engine.Evaluate(context.Background(), view, dirty)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			if len(opportunities) != test.want {
				t.Fatalf("opportunities = %d, want %d", len(opportunities), test.want)
			}
		})
	}
}

func TestSortOpportunitiesTieBreak(t *testing.T) {
	net := big.NewInt(10)
	start := testAddress(0x01)
	opportunities := []Opportunity{
		{
			Cycle: graph.Cycle{Pools: []domain.PoolKey{{Address: testAddress(0x20)}}, Start: start},
			Net:   net,
		},
		{
			Cycle: graph.Cycle{Pools: []domain.PoolKey{{Address: testAddress(0x10)}}, Start: start},
			Net:   net,
		},
	}

	sortOpportunities(opportunities)
	if opportunities[0].Cycle.Pools[0].Address != testAddress(0x10) {
		t.Fatalf("first pool = %s, want %s", opportunities[0].Cycle.Pools[0].Address, testAddress(0x10))
	}
}

func TestEvaluateCanceledContext(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 10, identityQuoter())
	view := newFakeView().add(poolA).add(poolB)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	opportunities, err := engine.Evaluate(ctx, view, []domain.PoolKey{poolA.Key})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if opportunities != nil {
		t.Fatalf("opportunities = %v, want nil", opportunities)
	}
}

func TestEvaluateNonPositiveNotionalSkipsCycle(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 0, 10, failQuoter(t))
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 0 {
		t.Fatalf("opportunities = %d, want 0", len(opportunities))
	}
}

func TestEvaluateQuoteErrorSkipsCycle(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	quoter := &fakeQuoter{quote: func(pool.State, pool.DeepState, domain.Address, *big.Int) (*big.Int, error) {
		return nil, errors.New("quote failed")
	}}
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 10, quoter)
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 0 {
		t.Fatalf("opportunities = %d, want 0", len(opportunities))
	}
	if len(quoter.calls) != 2 {
		t.Fatalf("quote calls = %d, want one attempt per candidate (2)", len(quoter.calls))
	}
}

func TestEvaluateQuoteErrorWhenContextCanceled(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	quoter := &fakeQuoter{quote: func(pool.State, pool.DeepState, domain.Address, *big.Int) (*big.Int, error) {
		cancel()
		return nil, errors.New("quote failed")
	}}
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 10, quoter)
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(ctx, view, []domain.PoolKey{poolA.Key})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if opportunities != nil {
		t.Fatalf("opportunities = %v, want nil", opportunities)
	}
}
