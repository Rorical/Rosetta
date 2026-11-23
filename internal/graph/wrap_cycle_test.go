package graph

import (
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// A V4 pool on native currency closes a loop from the wrapped token only
// through the wrap pool: WETH -> V3 -> USDT -> V4 -> ETH -> wrap -> WETH.
func TestWrapPoolClosesANativeV4Cycle(t *testing.T) {
	native, weth, usdt := domain.Address{}, testAddress(0x10), testAddress(0x20)
	v3 := pool.State{Key: domain.PoolKey{Address: testAddress(0x31), Kind: domain.PoolKindV3}, Token0: weth, Token1: usdt}
	v4 := pool.State{Key: domain.PoolKey{Address: testAddress(0x32), Kind: domain.PoolKindV4, PoolID: domain.Hash{1}}, Token0: native, Token1: usdt}
	wrap := pool.State{Key: domain.PoolKey{Address: weth, Kind: domain.PoolKindWrap}, Token0: native, Token1: weth}

	without := mustBuildTable(t, mustBuild(t, v3, v4), 4, 100)
	if without.Len() != 0 {
		t.Fatalf("%d cycles without the wrap pool", without.Len())
	}

	with := mustBuildTable(t, mustBuild(t, v3, v4, wrap), 4, 100)
	want := []domain.PoolKey{v3.Key, v4.Key, wrap.Key}
	slices.SortFunc(want, domain.ComparePoolKeys)
	found := 0
	for i := range with.Len() {
		cycle := with.Cycle(i)
		got := slices.Clone(cycle.Pools)
		slices.SortFunc(got, domain.ComparePoolKeys)
		if !slices.Equal(got, want) {
			t.Fatalf("unexpected cycle %v", cycle.Pools)
		}
		found++
	}
	// One loop, recorded once in each direction.
	if found != 2 {
		t.Fatalf("%d cycles through the wrap pool, want 2", found)
	}
	if candidates := with.Candidates([]domain.PoolKey{v4.Key}, 10); len(candidates) != 2 {
		t.Fatalf("the V4 pool's change selects %d cycles, want 2", len(candidates))
	}
}
