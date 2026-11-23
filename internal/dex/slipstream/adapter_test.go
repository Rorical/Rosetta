package slipstream

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func quoteTestState() pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: domain.Address{0x09},
			Kind:    domain.PoolKindSlipstream,
		},
		Token0:       domain.Address{0x01},
		Token1:       domain.Address{0x02},
		Fee:          500,
		TickSpacing:  10,
		SqrtPriceX96: math.Q96,
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000_000),
	}
}

func TestNew(t *testing.T) {
	tests := []struct {
		name        string
		feePips     uint32
		tickSpacing int32
		wantErr     bool
	}{
		{"zero fee", 0, 1, false},
		{"standard fee", 500, 10, false},
		{"fee at denominator", 1_000_000, 10, true},
		{"zero spacing", 500, 0, true},
		{"negative spacing", 500, -10, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := New(test.feePips, test.tickSpacing)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("error = %v, want ErrInvalidConfig", err)
				}
				if !errors.Is(err, uniswapv3.ErrInvalidConfig) {
					t.Fatalf("error = %v, want wrapped uniswapv3.ErrInvalidConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if adapter.Kind() != domain.PoolKindSlipstream {
				t.Fatalf("Kind = %d, want Slipstream", adapter.Kind())
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
		{"token0 in", domain.Address{0x01}, true},
		{"token1 in", domain.Address{0x02}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := quoteTestState()
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
			mutate:  func(s *pool.State) { s.Key.Kind = domain.PoolKindV4 },
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "v3 kind rejected",
			mutate:  func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 },
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "unknown token",
			tokenIn: domain.Address{0x03},
			amount:  big.NewInt(1),
			wantErr: uniswapv3.ErrTokenNotInPool,
		},
		{
			name:    "zero amount",
			tokenIn: domain.Address{0x01},
			amount:  new(big.Int),
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "zero liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = new(big.Int) },
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrNoLiquidity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := quoteTestState()
			if test.mutate != nil {
				test.mutate(&state)
			}
			adapter, err := New(state.Fee, state.TickSpacing)
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
	state := quoteTestState()
	adapter, err := New(state.Fee, state.TickSpacing)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := adapter.QuoteExactIn(ctx, state, pool.DeepState{}, state.Token0, big.NewInt(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
