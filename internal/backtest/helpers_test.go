package backtest

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func testAddress(id byte) domain.Address {
	var address domain.Address
	address[len(address)-1] = id
	return address
}

func testV2Pool(poolID, token0, token1 byte, reserve0, reserve1 int64) pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: testAddress(poolID),
			Kind:    domain.PoolKindV2,
		},
		Token0:   testAddress(token0),
		Token1:   testAddress(token1),
		Reserve0: big.NewInt(reserve0),
		Reserve1: big.NewInt(reserve1),
	}
}

func testBlock(number, timestamp uint64, apply func(*state.Store) error) Block {
	return Block{
		Ref:   domain.BlockRef{Number: number, Timestamp: timestamp},
		Apply: apply,
	}
}

// upsertPools returns an Apply that writes states into the store in order.
func upsertPools(states ...pool.State) func(*state.Store) error {
	return func(store *state.Store) error {
		for _, state := range states {
			if err := store.Upsert(state); err != nil {
				return err
			}
		}
		return nil
	}
}

// fakeQuoter prices legs with quote, counts calls, and optionally observes the
// call index so a test can trigger a side effect after a chosen leg.
type fakeQuoter struct {
	quote  func(state pool.State, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error)
	onCall func(call int)
	calls  int
}

func (q *fakeQuoter) QuoteExactIn(_ context.Context, state pool.State, _ pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	q.calls++
	if q.onCall != nil {
		q.onCall(q.calls)
	}
	return q.quote(state, tokenIn, amountIn)
}

// v2Quoter prices constant-product pools without fees, so each leg's output
// follows the reserves directly.
func v2Quoter() *fakeQuoter {
	return &fakeQuoter{quote: func(state pool.State, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
		reserveIn, reserveOut, ok := reserves(state, tokenIn)
		if !ok {
			return nil, fmt.Errorf("token %s not in pool %s", tokenIn, state.Key.Address)
		}
		out := new(big.Int).Mul(amountIn, reserveOut)
		return out.Quo(out, new(big.Int).Add(reserveIn, amountIn)), nil
	}}
}

func reserves(state pool.State, tokenIn domain.Address) (reserveIn, reserveOut *big.Int, ok bool) {
	switch tokenIn {
	case state.Token0:
		return state.Reserve0, state.Reserve1, true
	case state.Token1:
		return state.Reserve1, state.Reserve0, true
	default:
		return nil, nil, false
	}
}

func fixedNotional(amount int64) arb.NotionalFunc {
	return func(domain.Address) *big.Int {
		return big.NewInt(amount)
	}
}

func mustTable(t *testing.T, pools ...pool.State) *graph.Table {
	t.Helper()
	g, err := graph.Build(pools)
	if err != nil {
		t.Fatalf("graph.Build: %v", err)
	}
	table, err := graph.BuildTable(g, 2, 100)
	if err != nil {
		t.Fatalf("graph.BuildTable: %v", err)
	}
	return table
}

// mustRunner builds a Runner over pools with a constant-product quoter and a
// fixed notional. Cycles are enumerated to depth 2 and k is 2, so both
// directions of a two-pool cycle are candidates for either dirty pool.
func mustRunner(t *testing.T, pools ...pool.State) (*Runner, *fakeQuoter) {
	t.Helper()
	quoter := v2Quoter()
	runner, err := New(mustTable(t, pools...), quoter, fixedNotional(1_000), 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return runner, quoter
}

func requireNet(t *testing.T, step Step, index int, want int64) {
	t.Helper()
	if index >= len(step.Opportunities) {
		t.Fatalf("block %d: opportunities = %d, want at least %d", step.Ref.Number, len(step.Opportunities), index+1)
	}
	net := step.Opportunities[index].Net
	if net == nil || net.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("block %d opportunity %d net = %v, want %d", step.Ref.Number, index, net, want)
	}
}
