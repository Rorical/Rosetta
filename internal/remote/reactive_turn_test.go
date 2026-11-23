package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

// An intent whose first pool cannot lend borrows its principal from a
// standalone V3 flash loan, yet most such routes have a V3 pool further along
// that is entered with a configured asset. Measured on the executor, a
// MetricOmm then V3 loop costs 631,011 gas on a loan and 438,780 started on
// its V3 pool.
func TestFlashSwapTurnStartsTheLoopOnItsLendingPool(t *testing.T) {
	weth, usdc, other := addressAt(1), addressAt(2), addressAt(3)
	metric := domain.PoolKey{Kind: domain.PoolKindMetricOmm, Address: addressAt(10)}
	camelot := domain.PoolKey{Kind: domain.PoolKindCamelotV3, Address: addressAt(11)}
	v3 := domain.PoolKey{Kind: domain.PoolKindV3, Address: addressAt(20)}
	slip := domain.PoolKey{Kind: domain.PoolKindSlipstream, Address: addressAt(21)}
	store := poolstate.New()
	for _, p := range []pool.State{
		{Key: metric, Token0: weth, Token1: usdc, Metric: &pool.MetricState{Stale: true}},
		{Key: camelot, Token0: other, Token1: usdc, SqrtPriceX96: big.NewInt(1), Liquidity: big.NewInt(1)},
		{Key: v3, Token0: weth, Token1: usdc, Fee: 500, TickSpacing: 10, SqrtPriceX96: big.NewInt(1), Liquidity: big.NewInt(1)},
		{Key: slip, Token0: weth, Token1: other, Fee: 500, TickSpacing: 10, SqrtPriceX96: big.NewInt(1), Liquidity: big.NewInt(1)},
	} {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	view := store.View()
	legs := func(keys ...domain.PoolKey) []arb.Leg {
		out := make([]arb.Leg, len(keys))
		for i, key := range keys {
			out[i] = arb.Leg{Pool: key, AmountIn: big.NewInt(int64(100 * (i + 1))), AmountOut: big.NewInt(int64(100 * (i + 2)))}
		}
		return out
	}
	opportunity := func(start domain.Address, keys ...domain.PoolKey) arb.Opportunity {
		return arb.Opportunity{Cycle: graph.Cycle{Start: start, Pools: keys, Len: len(keys)}, Input: big.NewInt(100), Legs: legs(keys...)}
	}
	configured := func(tokens ...domain.Address) func(domain.Address) bool {
		return func(token domain.Address) bool {
			for _, t := range tokens {
				if t == token {
					return true
				}
			}
			return false
		}
	}

	// USDC -> MetricOmm -> WETH -> V3 -> USDC turns into WETH -> V3 -> USDC ->
	// MetricOmm -> WETH, entered with what the V3 leg took in.
	cycle, input, ok := flashSwapTurn(view, opportunity(usdc, metric, v3), configured(weth, usdc))
	if !ok || cycle.Start != weth || cycle.Len != 2 || cycle.Pools[0] != v3 || cycle.Pools[1] != metric || input.Int64() != 200 {
		t.Fatalf("turned to %+v with %v, %v", cycle, input, ok)
	}
	// The first lending pool entered with a configured asset is taken. Here
	// the Slipstream pool is entered with the unconfigured token and the V3
	// pool after it with WETH.
	cycle, input, ok = flashSwapTurn(view, opportunity(usdc, camelot, slip, v3), configured(weth, usdc))
	if !ok || cycle.Start != weth || cycle.Pools[0] != v3 || cycle.Pools[1] != camelot || cycle.Pools[2] != slip || input.Int64() != 300 {
		t.Fatalf("turned to %+v with %v, %v", cycle, input, ok)
	}
	for name, o := range map[string]arb.Opportunity{
		"already starts on a lending pool": opportunity(weth, v3, metric),
		"no lending pool":                  opportunity(usdc, metric, camelot),
		"no quoted legs":                   {Cycle: graph.Cycle{Start: usdc, Pools: []domain.PoolKey{metric, v3}, Len: 2}, Input: big.NewInt(100)},
	} {
		if cycle, _, ok := flashSwapTurn(view, o, configured(weth, usdc)); ok {
			t.Fatalf("%s: turned to %+v", name, cycle)
		}
	}
	if cycle, _, ok := flashSwapTurn(view, opportunity(usdc, metric, v3), configured(usdc)); ok {
		t.Fatalf("turned to a start that is not configured: %+v", cycle)
	}
}
