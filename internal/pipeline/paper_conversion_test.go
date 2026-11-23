package pipeline

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestOutsideRouteConversionUsesCurrentViewAndIndependentBridge(t *testing.T) {
	wrapped, bridge, asset := domain.Address{1}, domain.Address{2}, domain.Address{3}
	mk := func(id byte, a, c domain.Address, r0, r1 int64) pool.State {
		return pool.State{Key: domain.PoolKey{Address: domain.Address{id}, Kind: domain.PoolKindV2}, Token0: a, Token1: c, Reserve0: big.NewInt(r0), Reserve1: big.NewInt(r1)}
	}
	// The shallow, distorted direct bridge loses to the deeper pool. Neither is
	// required to occur in the arbitrage route.
	pools := []pool.State{mk(10, wrapped, bridge, 1, 1000000000000), mk(11, wrapped, bridge, 1000000, 2000000), mk(12, bridge, asset, 1000000, 3000000)}
	store := state.New()
	for _, p := range pools {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	b := newPaperBook(PaperCostPolicy{WrappedNative: wrapped.Hex()})
	b.prepareConversions(store.Pools())
	fee, keys, ok := b.convertOutsideRoute(store.View(), asset, wrapped, big.NewInt(10))
	// Capacity is measured in native units; the distorted pool's huge
	// output reserve must not masquerade as a deep native-liquidity bridge.
	if !ok || fee.Int64() != 60 || len(keys) != 2 || keys[0] != pools[1].Key {
		t.Fatalf("bridge got fee %v keys %v", fee, keys)
	}
	pools[1].Reserve1 = big.NewInt(4000000)
	if err := store.Upsert(pools[1]); err != nil {
		t.Fatal(err)
	}
	fee, _, ok = b.convertOutsideRoute(store.View(), asset, wrapped, big.NewInt(10))
	if !ok || fee.Int64() != 120 {
		t.Fatal("conversion reused old quote-state price", fee)
	}
	if _, _, ok = b.convertOutsideRoute(store.View(), domain.Address{99}, wrapped, big.NewInt(10)); ok {
		t.Fatal("invented absent conversion")
	}
}
