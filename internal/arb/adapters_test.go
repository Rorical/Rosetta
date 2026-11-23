package arb

import (
	"context"
	"math/big"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
)

// Concrete adapters are injected as Quoters; these assertions fail at compile
// time if their signatures drift from the engine contract.
var (
	_ Quoter = (*uniswapv2.Adapter)(nil)
	_ Quoter = (*uniswapv3.Adapter)(nil)
)

func TestEvaluateV2CycleWithConcreteAdapter(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA, poolB)
	adapter, err := uniswapv2.New(poolA.Fee)
	if err != nil {
		t.Fatalf("uniswapv2.New: %v", err)
	}
	engine := mustEngine(t, table, 1_000, 1, adapter)
	view := newFakeView().add(poolA).add(poolB)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}
	opportunity := opportunities[0]
	// The closed form resizes the 1_000 notional to the profit-maximising
	// input; the adapter must price exactly that amount.
	requireBig(t, opportunity.Input, 333_333, "Input")
	requireBig(t, opportunity.Output, 666_665, "Output")
	requireBig(t, opportunity.Net, 333_332, "Net")
	if len(opportunity.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(opportunity.Legs))
	}
	requireBig(t, opportunity.Legs[0].AmountOut, 499_999, "leg 0 out")
	requireBig(t, opportunity.Legs[1].AmountOut, 666_665, "leg 1 out")
}

func TestEvaluateV3CycleWithConcreteAdapter(t *testing.T) {
	poolA := testV3State(0x10, 0x01, 0x02, 3000, 10, 1_000_000_000)
	poolB := testV3State(0x20, 0x02, 0x01, 3000, 10, 1_000_000_000)
	deepA, deepB := emptyDeep(), emptyDeep()
	table := mustTable(t, 2, poolA, poolB)
	adapter, err := uniswapv3.New(poolA.Fee, poolA.TickSpacing)
	if err != nil {
		t.Fatalf("uniswapv3.New: %v", err)
	}
	engine := mustEngine(t, table, 50_000_000, 1, adapter)
	view := newFakeView().
		add(poolA).add(poolB).
		addDeep(poolA.Key, deepA).
		addDeep(poolB.Key, deepB)

	opportunities, err := engine.Evaluate(context.Background(), view, []domain.PoolKey{poolA.Key})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}

	ctx := context.Background()
	firstOut, err := adapter.QuoteExactIn(ctx, poolA, deepA, poolA.Token0, big.NewInt(50_000_000))
	if err != nil {
		t.Fatalf("first quote: %v", err)
	}
	secondOut, err := adapter.QuoteExactIn(ctx, poolB, deepB, poolB.Token0, firstOut)
	if err != nil {
		t.Fatalf("second quote: %v", err)
	}

	opportunity := opportunities[0]
	if len(opportunity.Legs) != 2 {
		t.Fatalf("legs = %d, want 2", len(opportunity.Legs))
	}
	requireBig(t, opportunity.Input, 50_000_000, "Input")
	if opportunity.Output.Cmp(secondOut) != 0 {
		t.Fatalf("Output = %s, want %s", opportunity.Output, secondOut)
	}
	wantNet := new(big.Int).Sub(secondOut, big.NewInt(50_000_000))
	if opportunity.Net.Cmp(wantNet) != 0 {
		t.Fatalf("Net = %s, want %s", opportunity.Net, wantNet)
	}
	if opportunity.Legs[0].AmountOut.Cmp(firstOut) != 0 {
		t.Fatalf("leg 0 out = %s, want %s", opportunity.Legs[0].AmountOut, firstOut)
	}
	if opportunity.Legs[1].AmountIn.Cmp(firstOut) != 0 {
		t.Fatalf("leg 1 in = %s, want %s", opportunity.Legs[1].AmountIn, firstOut)
	}
	if opportunity.Legs[1].AmountOut.Cmp(secondOut) != 0 {
		t.Fatalf("leg 1 out = %s, want %s", opportunity.Legs[1].AmountOut, secondOut)
	}
}

func TestEvaluateDeterministicSortWithV2Adapter(t *testing.T) {
	poolA1 := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolA2 := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	poolC1 := testV2State(0x30, 0x03, 0x04, 1_000_000, 2_000_000)
	poolC2 := testV2State(0x40, 0x04, 0x03, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA1, poolA2, poolC1, poolC2)
	adapter, err := uniswapv2.New(0)
	if err != nil {
		t.Fatalf("uniswapv2.New: %v", err)
	}
	engine := mustEngine(t, table, 1_000, 10, adapter)
	view := newFakeView().add(poolA1).add(poolA2).add(poolC1).add(poolC2)
	dirty := []domain.PoolKey{poolA1.Key, poolC1.Key}

	first, err := engine.Evaluate(context.Background(), view, dirty)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	second, err := engine.Evaluate(context.Background(), view, dirty)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}

	if len(first) != 4 {
		t.Fatalf("opportunities = %d, want 4", len(first))
	}
	want := []struct {
		start domain.Address
		net   int64
	}{
		{start: testAddress(0x01), net: 333_332},
		{start: testAddress(0x03), net: 333_332},
		{start: testAddress(0x01), net: -751},
		{start: testAddress(0x03), net: -751},
	}
	for i, expected := range want {
		if first[i].Cycle.Start != expected.start {
			t.Fatalf("opportunity %d Start = %s, want %s", i, first[i].Cycle.Start, expected.start)
		}
		requireBig(t, first[i].Net, expected.net, "opportunity Net")
	}

	firstSignatures := opportunitySignatures(first)
	secondSignatures := opportunitySignatures(second)
	if !slices.Equal(firstSignatures, secondSignatures) {
		t.Fatalf("order differs between runs: %v vs %v", firstSignatures, secondSignatures)
	}
}

func opportunitySignatures(opportunities []Opportunity) []string {
	signatures := make([]string, len(opportunities))
	for i, opportunity := range opportunities {
		signature := opportunity.Cycle.Start.Hex()
		for _, key := range opportunity.Cycle.Pools {
			signature += "|" + key.Address.Hex()
		}
		signatures[i] = signature + "|" + opportunity.Net.String()
	}
	return signatures
}
