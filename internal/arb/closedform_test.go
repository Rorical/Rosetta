package arb

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// TestEvaluateFallsBackWhenClosedFormUnprofitable keeps the fixed-notional path
// when the two legs price the same asset at the same rate, so the closed form
// has nothing to add.
func TestEvaluateFallsBackWhenClosedFormUnprofitable(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 2_000_000, 1_000_000)
	quoter := sequenceQuoter(t, 1_998, 3_988)
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 1, quoter)
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}
	requireBig(t, opportunities[0].Input, 1_000, "Input")
	requireBig(t, opportunities[0].Net, 2_988, "Net")
	if len(quoter.calls) != 2 {
		t.Fatalf("quote calls = %d, want only the two notional legs", len(quoter.calls))
	}
}

// TestEvaluateThreePoolCycleKeepsNotional proves only two-pool cycles are
// resized: a three-pool cycle is quoted once at the notional input.
func TestEvaluateThreePoolCycleKeepsNotional(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 1_000_000)
	poolB := testV2State(0x20, 0x02, 0x03, 1_000_000, 2_000_000)
	poolC := testV2State(0x30, 0x03, 0x01, 2_000_000, 1_000_000)
	quoter := sequenceQuoter(t, 2_000, 3_000, 4_000)
	engine := mustEngine(t, mustTable(t, 3, poolA, poolB, poolC), 1_000, 1, quoter)
	view := newFakeView().add(poolA).add(poolB).add(poolC)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}
	opportunity := opportunities[0]
	if len(opportunity.Legs) != 3 {
		t.Fatalf("legs = %d, want 3", len(opportunity.Legs))
	}
	requireBig(t, opportunity.Input, 1_000, "Input")
	requireBig(t, opportunity.Output, 4_000, "Output")
	requireBig(t, opportunity.Net, 3_000, "Net")
	if len(quoter.calls) != 3 {
		t.Fatalf("quote calls = %d, want only the three notional legs", len(quoter.calls))
	}
}

// TestEvaluateKeepsNotionalWhenClosedFormQuoteLoses covers the comparison
// itself: the model can be profitable while the exact quote at its input is
// worse than the notional quote.
func TestEvaluateKeepsNotionalWhenClosedFormQuoteLoses(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	quoter := constantQuoter(1_998)
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 1, quoter)
	view := newFakeView().add(poolA).add(poolB)

	opportunity := mustEvaluate(t, engine, view, []domain.PoolKey{poolA.Key})[0]
	requireBig(t, opportunity.Input, 1_000, "Input")
	requireBig(t, opportunity.Output, 1_998, "Output")
	requireBig(t, opportunity.Net, 998, "Net")
	if len(quoter.calls) != 4 {
		t.Fatalf("quote calls = %d, want the notional and closed-form attempts", len(quoter.calls))
	}
}

// TestEvaluateSkipsClosedFormWithoutReserves keeps the notional path when a
// leg's reserves are missing from shallow state.
func TestEvaluateSkipsClosedFormWithoutReserves(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	poolB.Reserve0 = nil
	quoter := identityQuoter()
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), 1_000, 1, quoter)
	view := newFakeView().add(poolA).add(poolB)

	opportunity := mustEvaluate(t, engine, view, []domain.PoolKey{poolA.Key})[0]
	requireBig(t, opportunity.Input, 1_000, "Input")
	requireBig(t, opportunity.Net, 0, "Net")
	if len(quoter.calls) != 2 {
		t.Fatalf("quote calls = %d, want only the notional legs", len(quoter.calls))
	}
}

// TestEvaluateV3ClosedFormUsesVirtualReserves checks that V3 legs are sized
// from the reserves implied by liquidity at the current price. The simulation
// below is deliberately explicit: at sqrtP = Q96 the reserves are (L, L); at
// sqrtP = 2*Q96 they are (L/2, 2L).
func TestEvaluateV3ClosedFormUsesVirtualReserves(t *testing.T) {
	const notional = 50_000_000
	const liquidity = 1_000_000_000

	poolA := testV3State(0x10, 0x01, 0x02, 3000, 10, liquidity)
	poolB := testV3State(0x20, 0x02, 0x01, 3000, 10, liquidity)
	poolB.SqrtPriceX96 = new(big.Int).Lsh(math.Q96, 1)
	tick, err := math.SqrtPriceX96ToTick(poolB.SqrtPriceX96)
	if err != nil {
		t.Fatalf("SqrtPriceX96ToTick: %v", err)
	}
	poolB.Tick = tick

	first := math.ClosedFormLeg{
		InReserve:  big.NewInt(liquidity),
		OutReserve: big.NewInt(liquidity),
		FeePips:    poolA.Fee,
	}
	second := math.ClosedFormLeg{
		InReserve:  big.NewInt(liquidity / 2),
		OutReserve: big.NewInt(2 * liquidity),
		FeePips:    poolB.Fee,
	}
	wantInput, _, _, profitable, err := math.ClosedFormTwoLeg(first, second)
	if err != nil {
		t.Fatalf("ClosedFormTwoLeg: %v", err)
	}
	if !profitable {
		t.Fatal("expected the synthetic V3 cycle to be profitable")
	}
	if wantInput.Cmp(big.NewInt(notional)) == 0 {
		t.Fatal("closed form returned the notional input; reserves were not used")
	}

	adapter, err := uniswapv3.New(poolA.Fee, poolA.TickSpacing)
	if err != nil {
		t.Fatalf("uniswapv3.New: %v", err)
	}
	deepA, deepB := emptyDeep(), emptyDeep()
	view := newFakeView().
		add(poolA).add(poolB).
		addDeep(poolA.Key, deepA).
		addDeep(poolB.Key, deepB)
	engine := mustEngine(t, mustTable(t, 2, poolA, poolB), notional, 1, adapter)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}
	opportunity := opportunities[0]
	if opportunity.Input.Cmp(wantInput) != 0 {
		t.Fatalf("Input = %s, want closed-form input %s", opportunity.Input, wantInput)
	}
	if notionalNet := quoteV3CycleNet(t, adapter, poolA, poolB, deepA, deepB, notional); opportunity.Net.Cmp(notionalNet) <= 0 {
		t.Fatalf("Net = %s, want more than the notional quote %s", opportunity.Net, notionalNet)
	}
}

// quoteV3CycleNet prices the same cycle at notional through the concrete V3
// adapter, independent of the engine's own quoting loop.
func quoteV3CycleNet(t *testing.T, adapter *uniswapv3.Adapter, first, second pool.State, deepFirst, deepSecond pool.DeepState, notional int64) *big.Int {
	t.Helper()
	ctx := context.Background()
	firstOut, err := adapter.QuoteExactIn(ctx, first, deepFirst, first.Token0, big.NewInt(notional))
	if err != nil {
		t.Fatalf("first leg: %v", err)
	}
	secondOut, err := adapter.QuoteExactIn(ctx, second, deepSecond, first.Token1, firstOut)
	if err != nil {
		t.Fatalf("second leg: %v", err)
	}
	return new(big.Int).Sub(secondOut, big.NewInt(notional))
}
