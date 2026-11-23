package arb

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type countingView struct {
	deep           pool.DeepState
	copies, shared int
}

func (v *countingView) Pool(domain.PoolKey) (pool.State, bool) { return pool.State{}, true }
func (v *countingView) Deep(domain.PoolKey) (pool.DeepState, bool) {
	v.copies++
	return v.deep.Copy(), true
}
func (v *countingView) SharedDeep(domain.PoolKey) (pool.DeepState, bool) {
	v.shared++
	return v.deep, true
}

// The quoter's cached view reads tick state once per pool and without a copy,
// through a carried view as well.
func TestCachedViewReadsTickStateWithoutCopy(t *testing.T) {
	key := domain.PoolKey{Address: domain.Address{1}}
	source := &countingView{deep: pool.DeepState{Ticks: map[int32]pool.TickInfo{60: {LiquidityGross: big.NewInt(5), LiquidityNet: big.NewInt(5)}}}}
	for name, view := range map[string]View{
		"direct":  CachedView(source),
		"carried": CachedView(&carriedView{current: source, basis: source, keys: map[domain.PoolKey]struct{}{key: {}}}),
	} {
		source.copies, source.shared = 0, 0
		first, ok := view.Deep(key)
		second, _ := view.Deep(key)
		if !ok || source.copies != 0 || source.shared != 1 {
			t.Fatalf("%s: copies %d, shared reads %d", name, source.copies, source.shared)
		}
		if first.Ticks[60].LiquidityNet != source.deep.Ticks[60].LiquidityNet || second.Ticks[60].LiquidityNet != first.Ticks[60].LiquidityNet {
			t.Fatalf("%s: tick state was copied", name)
		}
	}
}
