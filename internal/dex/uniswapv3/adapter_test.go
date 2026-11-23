package uniswapv3

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

var (
	testToken0 = domain.Address{0x01}
	testToken1 = domain.Address{0x02}
)

func TestNew(t *testing.T) {
	tests := []struct {
		name        string
		feePips     uint32
		tickSpacing int32
		wantErr     bool
	}{
		{"zero fee", 0, 1, false},
		{"standard fee", 3000, 60, false},
		{"max fee", 999_999, 60, false},
		{"fee at denominator", 1_000_000, 60, true},
		{"zero spacing", 3000, 0, true},
		{"negative spacing", 3000, -60, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := New(test.feePips, test.tickSpacing)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("error = %v, want ErrInvalidConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if adapter.feePips != test.feePips || adapter.tickSpacing != test.tickSpacing {
				t.Fatalf("adapter fields = (%d, %d), want (%d, %d)",
					adapter.feePips, adapter.tickSpacing, test.feePips, test.tickSpacing)
			}
			if adapter.Kind() != domain.PoolKindV3 {
				t.Fatalf("Kind = %d, want V3", adapter.Kind())
			}
		})
	}
}

func TestQuoteExactInMatchesSwapV3(t *testing.T) {
	deep := pool.DeepState{
		Bitmap: map[int16]*big.Int{},
		Ticks:  map[int32]pool.TickInfo{},
	}
	tests := []struct {
		name       string
		tokenIn    domain.Address
		zeroForOne bool
	}{
		{"token0 in", testToken0, true},
		{"token1 in", testToken1, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := quoteTestState(domain.PoolKindV3)
			adapter, err := New(state.Fee, state.TickSpacing)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			amountIn := big.NewInt(50_000_000)

			got, err := adapter.QuoteExactIn(context.Background(), state, deep, test.tokenIn, amountIn)
			if err != nil {
				t.Fatalf("QuoteExactIn: %v", err)
			}
			want, err := math.SwapV3ExactIn(math.V3State{
				SqrtPriceX96: state.SqrtPriceX96,
				Tick:         state.Tick,
				Liquidity:    state.Liquidity,
				FeePips:      state.Fee,
				TickSpacing:  state.TickSpacing,
				Bitmap:       deep.Bitmap,
				Ticks:        deep.Ticks,
			}, test.zeroForOne, amountIn)
			if err != nil {
				t.Fatalf("SwapV3ExactIn: %v", err)
			}
			if got.Cmp(want.AmountOut) != 0 {
				t.Fatalf("quote = %s, want %s", got, want.AmountOut)
			}
		})
	}
}

// TestQuoteExactInRejectsQuoteOutsideWindow checks the adapter forwards the
// loaded window to the swap, so a quote that would need ticks the caller did
// not load fails instead of inventing liquidity.
func TestQuoteExactInRejectsQuoteOutsideWindow(t *testing.T) {
	state := quoteTestState(domain.PoolKindV3)
	adapter, err := New(state.Fee, state.TickSpacing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	deep := pool.DeepState{
		Bitmap:    map[int16]*big.Int{},
		Ticks:     map[int32]pool.TickInfo{},
		WindowMin: 0,
		WindowMax: 1,
	}

	_, err = adapter.QuoteExactIn(context.Background(), state, deep, testToken0, big.NewInt(50_000_000))
	if !errors.Is(err, math.ErrOutOfWindow) {
		t.Fatalf("error = %v, want ErrOutOfWindow", err)
	}
}

func TestQuoteExactInValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*pool.State)
		tokenIn domain.Address
		amount  *big.Int
		wantErr error
	}{
		{
			name:    "wrong pool kind",
			mutate:  func(s *pool.State) { s.Key.Kind = domain.PoolKindV2 },
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "unknown token",
			tokenIn: domain.Address{0x03},
			amount:  big.NewInt(1),
			wantErr: ErrTokenNotInPool,
		},
		{
			name:    "nil amount",
			tokenIn: testToken0,
			amount:  nil,
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "zero amount",
			tokenIn: testToken0,
			amount:  new(big.Int),
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "negative amount",
			tokenIn: testToken0,
			amount:  big.NewInt(-1),
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "nil liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = nil },
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "zero liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = new(big.Int) },
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrNoLiquidity,
		},
		{
			name:    "negative liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = big.NewInt(-1) },
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := quoteTestState(domain.PoolKindV3)
			if test.mutate != nil {
				test.mutate(&state)
			}
			adapter, err := New(3000, 10)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = adapter.QuoteExactIn(context.Background(), state, pool.DeepState{}, test.tokenIn, test.amount)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestQuoteExactInRespectsCancellation(t *testing.T) {
	state := quoteTestState(domain.PoolKindV3)
	adapter, err := New(state.Fee, state.TickSpacing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := adapter.QuoteExactIn(ctx, state, pool.DeepState{}, testToken0, big.NewInt(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func quoteTestState(kind domain.PoolKind) pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: domain.Address{0x09},
			Kind:    kind,
		},
		Token0:       testToken0,
		Token1:       testToken1,
		Fee:          3000,
		TickSpacing:  10,
		SqrtPriceX96: new(big.Int).Set(math.Q96),
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000_000),
	}
}
