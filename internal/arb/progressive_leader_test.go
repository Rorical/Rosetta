package arb

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// An early batch can have weaker routes than a later batch. Spending the first
// depth quota must not force a new leader to wait for the complete table scan.
func TestProgressiveLaterLeaderUsesRemainingAssetBudget(t *testing.T) {
	pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000)}
	paths := [][]domain.PoolKey{}
	for id := byte(2); id < 9; id++ {
		pools = append(pools, testV2State(id, 2, 1, 1000000, 2000000))
		paths = append(paths, []domain.PoolKey{pools[0].Key, pools[len(pools)-1].Key})
	}
	engine := mustEngine(t, explicitTable(t, pools[0].Token0, paths...), 1000, 5, v2CurveQuoter())
	view := newFakeView()
	for _, p := range pools {
		view.add(p)
	}
	p := new(progressQuotes)
	candidate := func(index int, net int64) rankedCycle {
		return rankedCycle{cycle: engine.table.Cycle(index), input: big.NewInt(1000), net: big.NewInt(net)}
	}
	admit := func(index int, net int64) int {
		return len(p.admit(RankedSelection{candidates: []rankedCycle{candidate(index, net)}}, 2, engine.K()))
	}
	for index := range 4 {
		if admit(index, int64(index+1)) != 1 {
			t.Fatal("missing early representative")
		}
	}
	if admit(4, 3) != 0 {
		t.Fatal("weaker route spent reserved slot")
	}
	if admit(5, 5) != 1 {
		t.Fatal("new leader waited behind broad scan")
	}
	admitted := func() (total int) {
		for _, asset := range p.assets {
			total += asset.total
		}
		return total
	}
	if admit(6, 100) != 1 || admitted() != 6 {
		t.Fatal("bounded late-leader reserve was not available")
	}
}
