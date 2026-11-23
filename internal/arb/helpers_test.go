package arb

import (
	"context"
	"math/big"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func testAddress(id byte) domain.Address {
	var address domain.Address
	address[len(address)-1] = id
	return address
}

func testV2State(poolID, token0, token1 byte, reserve0, reserve1 int64) pool.State {
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

func testV3State(poolID, token0, token1 byte, fee uint32, tickSpacing int32, liquidity int64) pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: testAddress(poolID),
			Kind:    domain.PoolKindV3,
		},
		Token0:       testAddress(token0),
		Token1:       testAddress(token1),
		Fee:          fee,
		TickSpacing:  tickSpacing,
		SqrtPriceX96: new(big.Int).Set(math.Q96),
		Tick:         0,
		Liquidity:    big.NewInt(liquidity),
	}
}

func emptyDeep() pool.DeepState {
	return pool.DeepState{
		Bitmap: map[int16]*big.Int{},
		Ticks:  map[int32]pool.TickInfo{},
	}
}

type fakeView struct {
	states map[domain.PoolKey]pool.State
	deep   map[domain.PoolKey]pool.DeepState
}

func newFakeView() *fakeView {
	return &fakeView{
		states: make(map[domain.PoolKey]pool.State),
		deep:   make(map[domain.PoolKey]pool.DeepState),
	}
}

func (v *fakeView) add(state pool.State) *fakeView {
	v.states[state.Key] = state
	return v
}

func (v *fakeView) addDeep(key domain.PoolKey, deep pool.DeepState) *fakeView {
	v.deep[key] = deep
	return v
}

func (v *fakeView) Pool(key domain.PoolKey) (pool.State, bool) {
	state, ok := v.states[key]
	return state, ok
}

func (v *fakeView) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	deep, ok := v.deep[key]
	return deep, ok
}

func mustTable(t *testing.T, maxDepth int, states ...pool.State) *graph.Table {
	t.Helper()
	g, err := graph.Build(states)
	if err != nil {
		t.Fatalf("graph.Build: %v", err)
	}
	table, err := graph.BuildTable(g, maxDepth, 1000)
	if err != nil {
		t.Fatalf("graph.BuildTable: %v", err)
	}
	return table
}

func fixedNotional(amount int64) NotionalFunc {
	return func(domain.Address) *big.Int {
		return big.NewInt(amount)
	}
}

// quoteCall records one leg request made to a fakeQuoter.
type quoteCall struct {
	pool   domain.Address
	token  domain.Address
	amount *big.Int
}

// fakeQuoter prices legs with quote and records every call in order.
type fakeQuoter struct {
	mu    sync.Mutex
	quote func(state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error)
	calls []quoteCall
}

func (q *fakeQuoter) QuoteExactIn(_ context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	q.mu.Lock()
	q.calls = append(q.calls, quoteCall{
		pool:   state.Key.Address,
		token:  tokenIn,
		amount: new(big.Int).Set(amountIn),
	})
	q.mu.Unlock()
	return q.quote(state, deep, tokenIn, amountIn)
}

// identityQuoter returns the input unchanged.
func identityQuoter() *fakeQuoter {
	return &fakeQuoter{quote: func(_ pool.State, _ pool.DeepState, _ domain.Address, amountIn *big.Int) (*big.Int, error) {
		return new(big.Int).Set(amountIn), nil
	}}
}

// v2CurveQuoter prices constant-product legs from each pool's reserves and fee,
// matching the curve the closed form models.
func v2CurveQuoter() *fakeQuoter {
	return &fakeQuoter{quote: func(state pool.State, _ pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
		reserveIn, reserveOut := state.Reserve0, state.Reserve1
		if tokenIn == state.Token1 {
			reserveIn, reserveOut = reserveOut, reserveIn
		}
		return math.QuoteV2ExactIn(reserveIn, reserveOut, state.Fee, amountIn)
	}}
}

// quoteCycleNet prices a two-pool cycle from first.Token0 through the pool
// pairing first.Token1 with second, and returns output minus input.
func quoteCycleNet(t *testing.T, first, second pool.State, amountIn int64) *big.Int {
	t.Helper()
	quoter := v2CurveQuoter()
	ctx := context.Background()
	firstOut, err := quoter.QuoteExactIn(ctx, first, pool.DeepState{}, first.Token0, big.NewInt(amountIn))
	if err != nil {
		t.Fatalf("first leg: %v", err)
	}
	secondOut, err := quoter.QuoteExactIn(ctx, second, pool.DeepState{}, first.Token1, firstOut)
	if err != nil {
		t.Fatalf("second leg: %v", err)
	}
	return new(big.Int).Sub(secondOut, big.NewInt(amountIn))
}

// constantQuoter returns amount for every leg.
func constantQuoter(amount int64) *fakeQuoter {
	return &fakeQuoter{quote: func(pool.State, pool.DeepState, domain.Address, *big.Int) (*big.Int, error) {
		return big.NewInt(amount), nil
	}}
}

// sequenceQuoter returns the scripted outputs in call order.
func sequenceQuoter(t *testing.T, amounts ...int64) *fakeQuoter {
	t.Helper()
	index := 0
	return &fakeQuoter{quote: func(pool.State, pool.DeepState, domain.Address, *big.Int) (*big.Int, error) {
		if index >= len(amounts) {
			t.Fatalf("quote call %d beyond %d scripted outputs", index+1, len(amounts))
		}
		amount := amounts[index]
		index++
		return big.NewInt(amount), nil
	}}
}

// failQuoter fails the test if the engine asks for a quote.
func failQuoter(t *testing.T) *fakeQuoter {
	t.Helper()
	return &fakeQuoter{quote: func(pool.State, pool.DeepState, domain.Address, *big.Int) (*big.Int, error) {
		t.Fatal("unexpected quote call")
		return nil, nil
	}}
}

func mustEngine(t *testing.T, table *graph.Table, notional int64, k int, quoter Quoter) *Engine {
	t.Helper()
	engine, err := New(table, fixedNotional(notional), k, quoter)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return engine
}

func requireBig(t *testing.T, got *big.Int, want int64, label string) {
	t.Helper()
	if got == nil || got.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("%s = %v, want %d", label, got, want)
	}
}
