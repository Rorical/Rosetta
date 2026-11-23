package planning

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// The floor is the builder's own budget for the cheapest funding mode: base
// fee with headroom times the gas the route is expected to use.
func TestNetworkCostFloorIsTheCheapestFundingBudget(t *testing.T) {
	two := []domain.PoolKey{{Address: domain.Address{1}}, {Address: domain.Address{2}}}
	four := append(append([]domain.PoolKey{}, two...), domain.PoolKey{Address: domain.Address{3}}, domain.PoolKey{Address: domain.Address{4}})
	costs := ReactiveCosts{BaseFee: big.NewInt(20_000_000), Extra: new(big.Int)}
	cfg := ReactivePolicy{GasLimit: 2_000_000, RouteGasBase: 350_000, RouteGasPerStep: 300_000, BaseFeeHeadroomBPS: 15000}
	// 30,000,000 wei per gas after headroom.
	if got := NetworkCostFloor(two, costs, cfg); got.Cmp(big.NewInt(950_000*30_000_000)) != 0 {
		t.Fatalf("two pools: %s", got)
	}
	if got := NetworkCostFloor(four, costs, cfg); got.Cmp(big.NewInt(1_550_000*30_000_000)) != 0 {
		t.Fatalf("four pools: %s", got)
	}
	// A cheaper funding mode and an expected-gas estimate both lower it.
	cfg.GasByFunding = map[execution.Funding]uint64{execution.FundingWallet: 400_000}
	cfg.TipGas = func(_ []domain.PoolKey, _ execution.Funding, limit uint64) uint64 { return limit / 2 }
	if got := NetworkCostFloor(two, costs, cfg); got.Cmp(big.NewInt(200_000*30_000_000)) != 0 {
		t.Fatalf("wallet funding at half its limit: %s", got)
	}
	if got := NetworkCostFloor(two, ReactiveCosts{}, cfg); got.Sign() != 0 {
		t.Fatalf("no fee context: %s", got)
	}
}
