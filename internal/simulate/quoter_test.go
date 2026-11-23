package simulate

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// fakeAdapter records the quote arguments and returns a canned result.
type fakeAdapter struct {
	kind      domain.PoolKind
	amountOut *big.Int
	err       error
	calls     int
	lastCtx   context.Context
	lastState pool.State
	lastDeep  pool.DeepState
	lastToken domain.Address
	lastIn    *big.Int
}

func (a *fakeAdapter) Kind() domain.PoolKind { return a.kind }

func (a *fakeAdapter) QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	a.calls++
	a.lastCtx = ctx
	a.lastState = state
	a.lastDeep = deep
	a.lastToken = tokenIn
	a.lastIn = amountIn
	if a.err != nil {
		return nil, a.err
	}
	return a.amountOut, nil
}

func testState(kind domain.PoolKind, address byte) pool.State {
	return pool.State{
		Key:          domain.PoolKey{Address: domain.Address{address}, Kind: kind},
		Token0:       domain.Address{0x01},
		Token1:       domain.Address{0x02},
		Fee:          3000,
		TickSpacing:  10,
		SqrtPriceX96: big.NewInt(1 << 60),
		Tick:         -5,
		Liquidity:    big.NewInt(1_000_000),
		Reserve0:     big.NewInt(2_000),
		Reserve1:     big.NewInt(3_000),
		UpdatedBlock: 42,
	}
}

func testDeep() pool.DeepState {
	return pool.DeepState{
		Bitmap: map[int16]*big.Int{-1: big.NewInt(7)},
		Ticks: map[int32]pool.TickInfo{
			-60: {LiquidityGross: big.NewInt(10), LiquidityNet: big.NewInt(-10)},
		},
	}
}

func mustQuoter(t *testing.T, factory AdapterFactory) *Quoter {
	t.Helper()
	quoter, err := NewQuoter(factory)
	if err != nil {
		t.Fatalf("NewQuoter: %v", err)
	}
	return quoter
}

func TestQuoteExactInDelegatesToAdapter(t *testing.T) {
	wantState := testState(domain.PoolKindV3, 0x10)
	state := wantState
	deep := testDeep()
	wantDeep := testDeep()
	amountIn := big.NewInt(1_234)
	wantOut := big.NewInt(5_678)
	adapter := &fakeAdapter{kind: domain.PoolKindV3, amountOut: wantOut}
	ctx := context.Background()
	tokenIn := domain.Address{0x01}

	quoter := mustQuoter(t, func(got pool.State) (pool.Adapter, error) {
		if !reflect.DeepEqual(got, wantState) {
			t.Fatalf("factory state = %+v, want %+v", got, wantState)
		}
		return adapter, nil
	})

	gotOut, err := quoter.QuoteExactIn(ctx, state, deep, tokenIn, amountIn)
	if err != nil {
		t.Fatalf("QuoteExactIn: %v", err)
	}
	if gotOut.Cmp(wantOut) != 0 {
		t.Fatalf("amount out = %s, want %s", gotOut, wantOut)
	}
	if adapter.calls != 1 {
		t.Fatalf("adapter calls = %d, want 1", adapter.calls)
	}
	if adapter.lastCtx != ctx {
		t.Fatalf("adapter ctx = %v, want the caller's context", adapter.lastCtx)
	}
	if !reflect.DeepEqual(adapter.lastState, wantState) {
		t.Fatalf("adapter state = %+v, want %+v", adapter.lastState, wantState)
	}
	if !reflect.DeepEqual(adapter.lastDeep, wantDeep) {
		t.Fatalf("adapter deep = %+v, want %+v", adapter.lastDeep, wantDeep)
	}
	if adapter.lastToken != tokenIn {
		t.Fatalf("adapter token in = %s, want %s", adapter.lastToken, tokenIn)
	}
	if adapter.lastIn.Cmp(amountIn) != 0 {
		t.Fatalf("adapter amount in = %s, want %s", adapter.lastIn, amountIn)
	}
	if !reflect.DeepEqual(state, wantState) {
		t.Fatalf("state mutated: %+v, want %+v", state, wantState)
	}
	if !reflect.DeepEqual(deep, wantDeep) {
		t.Fatalf("deep mutated: %+v, want %+v", deep, wantDeep)
	}
}

func TestQuoteExactInPropagatesErrors(t *testing.T) {
	factoryErr := errors.New("factory failed")
	adapterErr := errors.New("adapter failed")
	failingAdapter := &fakeAdapter{err: adapterErr}

	tests := []struct {
		name             string
		factory          AdapterFactory
		wantErr          error
		wantAdapterCalls int
	}{
		{
			name: "factory error",
			factory: func(pool.State) (pool.Adapter, error) {
				return nil, factoryErr
			},
			wantErr: factoryErr,
		},
		{
			name: "adapter error",
			factory: func(pool.State) (pool.Adapter, error) {
				return failingAdapter, nil
			},
			wantErr:          adapterErr,
			wantAdapterCalls: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failingAdapter.calls = 0
			quoter := mustQuoter(t, tt.factory)

			_, err := quoter.QuoteExactIn(context.Background(), testState(domain.PoolKindV2, 0x10), pool.DeepState{}, domain.Address{0x01}, big.NewInt(1))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, tt.wantErr)
			}
			if !strings.HasPrefix(err.Error(), "simulate:") {
				t.Fatalf("err = %q, want simulate: context", err)
			}
			if failingAdapter.calls != tt.wantAdapterCalls {
				t.Fatalf("adapter calls = %d, want %d", failingAdapter.calls, tt.wantAdapterCalls)
			}
		})
	}
}

func TestNewQuoterRejectsNilFactory(t *testing.T) {
	quoter, err := NewQuoter(nil)
	if err == nil {
		t.Fatal("NewQuoter(nil) error = nil, want error")
	}
	if quoter != nil {
		t.Fatalf("NewQuoter(nil) quoter = %v, want nil", quoter)
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want %v", err, ErrInvalidConfig)
	}
}

func TestQuoteExactInRejectsNilAdapterFromFactory(t *testing.T) {
	state := testState(domain.PoolKindV2, 0x10)
	quoter := mustQuoter(t, func(pool.State) (pool.Adapter, error) {
		return nil, nil
	})

	_, err := quoter.QuoteExactIn(context.Background(), state, pool.DeepState{}, domain.Address{0x01}, big.NewInt(1))
	if err == nil {
		t.Fatal("QuoteExactIn error = nil, want error")
	}
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("err = %v, want %v", err, ErrInvalidConfig)
	}
	if !strings.Contains(err.Error(), state.Key.Address.Hex()) {
		t.Fatalf("err = %q, want pool address %s", err, state.Key.Address.Hex())
	}
}

func TestQuoteExactInStopsOnDoneContext(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()

	tests := []struct {
		name    string
		ctx     context.Context
		wantErr error
	}{
		{name: "canceled", ctx: canceled, wantErr: context.Canceled},
		{name: "expired deadline", ctx: expired, wantErr: context.DeadlineExceeded},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			factoryCalls := 0
			quoter := mustQuoter(t, func(pool.State) (pool.Adapter, error) {
				factoryCalls++
				return &fakeAdapter{amountOut: big.NewInt(1)}, nil
			})

			_, err := quoter.QuoteExactIn(tt.ctx, testState(domain.PoolKindV2, 0x10), pool.DeepState{}, domain.Address{0x01}, big.NewInt(1))
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if factoryCalls != 0 {
				t.Fatalf("factory calls = %d, want 0", factoryCalls)
			}
		})
	}
}

func TestQuoteExactInSelectsAdapterByPoolKind(t *testing.T) {
	v2Adapter := &fakeAdapter{kind: domain.PoolKindV2, amountOut: big.NewInt(2_000)}
	v3Adapter := &fakeAdapter{kind: domain.PoolKindV3, amountOut: big.NewInt(3_000)}
	quoter := mustQuoter(t, func(state pool.State) (pool.Adapter, error) {
		switch state.Key.Kind {
		case domain.PoolKindV2:
			return v2Adapter, nil
		case domain.PoolKindV3:
			return v3Adapter, nil
		default:
			return nil, errors.New("unsupported pool kind")
		}
	})

	tests := []struct {
		name        string
		kind        domain.PoolKind
		wantOut     int64
		wantV2Calls int
		wantV3Calls int
	}{
		{name: "v2 pool", kind: domain.PoolKindV2, wantOut: 2_000, wantV2Calls: 1, wantV3Calls: 0},
		{name: "v3 pool", kind: domain.PoolKindV3, wantOut: 3_000, wantV2Calls: 0, wantV3Calls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v2Adapter.calls, v3Adapter.calls = 0, 0

			gotOut, err := quoter.QuoteExactIn(context.Background(), testState(tt.kind, 0x10), testDeep(), domain.Address{0x01}, big.NewInt(1))
			if err != nil {
				t.Fatalf("QuoteExactIn: %v", err)
			}
			if gotOut.Int64() != tt.wantOut {
				t.Fatalf("amount out = %s, want %d", gotOut, tt.wantOut)
			}
			if v2Adapter.calls != tt.wantV2Calls || v3Adapter.calls != tt.wantV3Calls {
				t.Fatalf("adapter calls = (v2 %d, v3 %d), want (%d, %d)",
					v2Adapter.calls, v3Adapter.calls, tt.wantV2Calls, tt.wantV3Calls)
			}
		})
	}
}
