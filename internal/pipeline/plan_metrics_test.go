package pipeline

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// One native token buys 2000 of a six-decimal asset, so 1e18 wei maps to 2e9
// base units: Numerator/Denominator is asset per wei.
var usdConversion = execution.FeeConversion{Numerator: big.NewInt(2_000_000_000), Denominator: new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)}

func estimatePlan(pool byte, net int64) *execution.ReadyPlan {
	return &execution.ReadyPlan{
		CreatedUnixMS: 1000, ExpiresUnixMS: 1250,
		Route: execution.ExecutorPlan{Asset: domain.Address{1}, Amount: big.NewInt(net), Steps: []execution.ExecutorStep{{Pool: domain.PoolKey{Address: domain.Address{pool}, Kind: domain.PoolKindV3}}}},
		Evidence: execution.ExecutionEvidence{Profit: big.NewInt(net + 2_000_000), GasUsed: 300_000, GasUsedForL1: 40_000,
			TotalFeeWei: big.NewInt(3e15), L1FeeWei: big.NewInt(2e15), L2FeeWei: big.NewInt(1e15)},
		Economics: execution.Economics{Conversion: usdConversion, ActualNetProfit: big.NewInt(net), BudgetNetProfit: big.NewInt(net / 2)},
	}
}

func TestPlanEstimatesConvertToNativeAndCountARouteOnce(t *testing.T) {
	registry := &telemetry.Registry{}
	// 2,000,000 base units is 2 of the asset, or 0.001 native: 1,000,000 gwei.
	first := estimatePlan(7, 2_000_000)
	ObserveDelivered(registry, telemetry.Standard, first, time.Now())
	// The same route reconfirmed on the next block, at a new size.
	ObserveDelivered(registry, telemetry.Standard, estimatePlan(7, 4_000_000), time.Time{})
	source := registry.Snapshot().Sources["standard"]
	if source.Counters["ready_plans"] != 2 || source.Stages["ready_latency"].Count != 1 {
		t.Fatalf("every plan must still count: %+v", source.Counters["ready_plans"])
	}
	for item, want := range map[string]uint64{
		"routes": 1, "net_profit_gwei": 1_000_000, "budget_net_profit_gwei": 500_000, "gross_profit_gwei": 2_000_000,
		"gas_used": 300_000, "l1_gas_used": 40_000, "fee_gwei": 3_000_000, "l1_fee_gwei": 2_000_000, "l2_fee_gwei": 1_000_000,
	} {
		if source.Estimates[item] != want {
			t.Errorf("%s = %d, want %d", item, source.Estimates[item], want)
		}
	}
	ObserveDelivered(registry, telemetry.Standard, estimatePlan(8, 2_000_000), time.Time{})
	if got := registry.Snapshot().Sources["standard"].Estimates["net_profit_gwei"]; got != 2_000_000 {
		t.Fatalf("new route not added: %d", got)
	}
	if registry.Snapshot().Sources["fast"].Estimates["routes"] != 0 {
		t.Fatal("estimates leaked between sources")
	}
	if registry.Snapshot().Sources["standard"].Stages["plan_validity"].SumNS != uint64(3*250*time.Millisecond) {
		t.Fatal("plan validity not observed")
	}
}

func TestHeldPlansStayOutOfDeliveredTotals(t *testing.T) {
	registry := &telemetry.Registry{}
	ObserveHeld(registry, telemetry.Fast, estimatePlan(7, 2_000_000))
	fast := registry.Snapshot().Sources["fast"]
	if fast.Counters["plans_held"] != 1 || fast.Estimates["held_routes"] != 1 || fast.Estimates["held_net_profit_gwei"] != 1_000_000 ||
		fast.Estimates["net_profit_gwei"] != 0 || fast.Estimates["routes"] != 0 {
		t.Fatalf("held estimates: %+v", fast.Estimates)
	}
}

func TestGweiConversionEdges(t *testing.T) {
	for _, test := range []struct {
		name   string
		amount *big.Int
		conv   execution.FeeConversion
		want   uint64
	}{
		{"nil amount", nil, usdConversion, 0},
		{"loss clamps", big.NewInt(-5), usdConversion, 0},
		{"missing conversion", big.NewInt(5), execution.FeeConversion{}, 0},
		{"overflow saturates", new(big.Int).Lsh(big.NewInt(1), 200), usdConversion, ^uint64(0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := assetGwei(test.amount, test.conv); got != test.want {
				t.Fatalf("got %d", got)
			}
		})
	}
}

func TestDisabledRegistryIgnoresPlans(t *testing.T) {
	var disabled *telemetry.Registry
	ObserveDelivered(disabled, telemetry.Fast, estimatePlan(1, 1), time.Now())
	ObserveHeld(disabled, telemetry.Fast, estimatePlan(1, 1))
}
