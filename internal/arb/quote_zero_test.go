package arb

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestRoundedZeroOutputIsLossWithoutDownstreamDependencies(t *testing.T) {
	for _, kind := range []domain.PoolKind{domain.PoolKindV2, domain.PoolKindV3} {
		t.Run(map[domain.PoolKind]string{domain.PoolKindV2: "zero input", domain.PoolKindV3: "missing ticks"}[kind], func(t *testing.T) {
			first := testV2State(10, 1, 2, 1_000_000, 1)
			second := testV2State(20, 2, 1, 1, 1_000_000)
			if kind == domain.PoolKindV3 {
				second = testV3State(20, 2, 1, 3000, 60, 1_000_000)
			}
			table := explicitTable(t, first.Token0, []domain.PoolKey{first.Key, second.Key})
			quoter := v2CurveQuoter()
			engine := mustEngine(t, table, 1, 1, quoter)
			input := big.NewInt(1)
			out, err := engine.QuoteCycle(t.Context(), newFakeView().add(first).add(second), table.Cycle(0), input)
			if err != nil {
				t.Fatalf("rounding loss became a dependency failure: %v", err)
			}
			if out.Output.Sign() != 0 || out.Net.Cmp(big.NewInt(-1)) != 0 || input.Int64() != 1 {
				t.Fatal("wrong loss or input mutated", out)
			}
			if len(quoter.calls) != 1 {
				t.Fatal("quoted an unreachable downstream leg", len(quoter.calls))
			}
		})
	}
}
