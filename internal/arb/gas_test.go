package arb

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// TestEvaluateGasAccounting checks that gas is reported next to the pre-gas
// net and never changes the chosen input or gross net.
func TestEvaluateGasAccounting(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	quoter := v2CurveQuoter()
	table := mustTable(t, 2, poolA, poolB)
	plain := mustEngine(t, table, 1_000, 1, quoter)
	gassed := plain.WithGas(GasConfig{Units: 21_000, Price: big.NewInt(2), Token: testAddress(0x01)})
	if gassed == plain {
		t.Fatal("WithGas returned the receiver, want a copy")
	}
	view := newFakeView().add(poolA).add(poolB)
	dirty := []domain.PoolKey{poolA.Key}

	before := mustEvaluate(t, plain, view, dirty)[0]
	after := mustEvaluate(t, gassed, view, dirty)[0]

	if before.Net.Cmp(after.Net) != 0 {
		t.Fatalf("Net changed with gas: %s -> %s", before.Net, after.Net)
	}
	if before.Input.Cmp(after.Input) != 0 {
		t.Fatalf("Input changed with gas: %s -> %s", before.Input, after.Input)
	}
	requireBig(t, before.GasCost, 0, "GasCost without WithGas")
	if before.NetAfterGas.Cmp(before.Net) != 0 {
		t.Fatalf("NetAfterGas = %s, want Net %s", before.NetAfterGas, before.Net)
	}
	if !before.GasComparable {
		t.Fatal("GasComparable = false without gas, want true")
	}

	requireBig(t, after.GasCost, 42_000, "GasCost")
	wantNetAfterGas := new(big.Int).Sub(after.Net, big.NewInt(42_000))
	if after.NetAfterGas.Cmp(wantNetAfterGas) != 0 {
		t.Fatalf("NetAfterGas = %s, want Net-GasCost = %s", after.NetAfterGas, wantNetAfterGas)
	}
	if after.NetAfterGas.Cmp(after.Net) >= 0 {
		t.Fatalf("NetAfterGas = %s, want below Net %s", after.NetAfterGas, after.Net)
	}
	if !after.GasComparable {
		t.Fatal("GasComparable = false for a cycle starting in the gas token")
	}
}

// TestEvaluateGasDifferentToken checks that a cost in another token is
// reported but never subtracted: NetAfterGas stays the pre-gas net.
func TestEvaluateGasDifferentToken(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA, poolB)
	engine := mustEngine(t, table, 1_000, 1, v2CurveQuoter()).
		WithGas(GasConfig{Units: 21_000, Price: big.NewInt(2), Token: testAddress(0x99)})
	view := newFakeView().add(poolA).add(poolB)

	opportunity := mustEvaluate(t, engine, view, []domain.PoolKey{poolA.Key})[0]
	requireBig(t, opportunity.GasCost, 42_000, "GasCost")
	if opportunity.NetAfterGas.Cmp(opportunity.Net) != 0 {
		t.Fatalf("NetAfterGas = %s, want pre-gas Net %s", opportunity.NetAfterGas, opportunity.Net)
	}
	if opportunity.GasComparable {
		t.Fatal("GasComparable = true for a cycle starting in another token")
	}
}

// TestWithGasNormalizesPrice treats a nil or negative price as free gas, since
// a negative cost would inflate profit.
func TestWithGasNormalizesPrice(t *testing.T) {
	poolA := testV2State(0x10, 0x01, 0x02, 1_000_000, 2_000_000)
	poolB := testV2State(0x20, 0x02, 0x01, 1_000_000, 2_000_000)
	table := mustTable(t, 2, poolA, poolB)
	view := newFakeView().add(poolA).add(poolB)
	dirty := []domain.PoolKey{poolA.Key}

	tests := []struct {
		name           string
		cfg            GasConfig
		want           int64
		wantComparable bool
	}{
		{name: "zero value", cfg: GasConfig{}, wantComparable: true},
		{name: "nil price", cfg: GasConfig{Units: 100, Token: testAddress(0x01)}, wantComparable: true},
		{name: "negative price", cfg: GasConfig{Units: 100, Price: big.NewInt(-2), Token: testAddress(0x01)}, wantComparable: true},
		{name: "zero units", cfg: GasConfig{Price: big.NewInt(7), Token: testAddress(0x01)}, wantComparable: true},
		{name: "free gas in another token", cfg: GasConfig{Units: 100, Token: testAddress(0x99)}, wantComparable: true},
		{
			name:           "units times price",
			cfg:            GasConfig{Units: 21_000, Price: big.NewInt(3), Token: testAddress(0x01)},
			want:           63_000,
			wantComparable: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine := mustEngine(t, table, 1_000, 1, v2CurveQuoter()).WithGas(test.cfg)
			opportunity := mustEvaluate(t, engine, view, dirty)[0]
			requireBig(t, opportunity.GasCost, test.want, "GasCost")
			wantNetAfterGas := new(big.Int).Sub(opportunity.Net, big.NewInt(test.want))
			if opportunity.NetAfterGas.Cmp(wantNetAfterGas) != 0 {
				t.Fatalf("NetAfterGas = %s, want %s", opportunity.NetAfterGas, wantNetAfterGas)
			}
			if opportunity.GasComparable != test.wantComparable {
				t.Fatalf("GasComparable = %t, want %t", opportunity.GasComparable, test.wantComparable)
			}
		})
	}
}

func mustEvaluate(t *testing.T, engine *Engine, view View, dirty []domain.PoolKey) []Opportunity {
	t.Helper()
	opportunities, err := engine.Evaluate(context.Background(), view, dirty)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if len(opportunities) == 0 {
		t.Fatal("opportunities = 0, want at least one")
	}
	return opportunities
}
