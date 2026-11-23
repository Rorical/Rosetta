package uniswapv3

import (
	"context"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Reactive builds share one pool copy across every quote (arb.CachedView), so
// a quote that crosses ticks must leave the state and tick data it read
// unchanged and give the same answer as a quote on a fresh copy.
func TestQuoteExactInLeavesSharedStateUnchanged(t *testing.T) {
	state := quoteTestState(domain.PoolKindV3)
	state.TickSpacing = 10
	state.Liquidity = new(big.Int)
	deep := &pool.DeepState{}
	for _, m := range []struct {
		lower, upper int32
		amount       int64
	}{{-20, 20, 2_000_000_000_000_000_000}, {-40, -20, 1_000_000_000_000_000_000}, {20, 60, 1_500_000_000_000_000_000}} {
		if err := Apply(&state, deep, Event{Kind: EventMint, TickLower: m.lower, TickUpper: m.upper, Amount: big.NewInt(m.amount)}); err != nil {
			t.Fatal(err)
		}
	}
	adapter, err := New(state.Fee, state.TickSpacing)
	if err != nil {
		t.Fatal(err)
	}
	beforeState, beforeDeep := state.Copy(), deep.Copy()
	quotes := []struct {
		token  [20]byte
		amount *big.Int
	}{{testToken0, big.NewInt(2_500_000_000_000_000)}, {testToken1, big.NewInt(4_000_000_000_000_000)}, {testToken0, big.NewInt(2_900_000_000_000_000)}, {testToken0, big.NewInt(2_500_000_000_000_000)}}
	for i, q := range quotes {
		shared, err := adapter.QuoteExactIn(context.Background(), state, *deep, q.token, q.amount)
		if err != nil {
			t.Fatal(i, err)
		}
		fresh, err := adapter.QuoteExactIn(context.Background(), beforeState.Copy(), beforeDeep.Copy(), q.token, q.amount)
		if err != nil {
			t.Fatal(i, err)
		}
		if shared.Cmp(fresh) != 0 {
			t.Fatalf("quote %d on shared state %s, on a fresh copy %s", i, shared, fresh)
		}
	}
	if !reflect.DeepEqual(state, beforeState) || !reflect.DeepEqual(*deep, beforeDeep) {
		t.Fatal("quoting mutated the pool state or its ticks")
	}
}
