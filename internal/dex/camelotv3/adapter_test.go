package camelotv3

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func quoteTestState() pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: domain.Address{0x09},
			Kind:    domain.PoolKindCamelotV3,
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

func TestAdapterKind(t *testing.T) {
	adapter := &Adapter{FeePips: 500, TickSpacing: 10}
	if adapter.Kind() != domain.PoolKindCamelotV3 {
		t.Fatalf("Kind = %d, want Camelot V3", adapter.Kind())
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
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if adapter.Kind() != domain.PoolKindCamelotV3 {
				t.Fatalf("Kind = %d, want Camelot V3", adapter.Kind())
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
			adapter := &Adapter{FeePips: state.Fee, TickSpacing: state.TickSpacing}
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
		adapter *Adapter
		tokenIn domain.Address
		amount  *big.Int
		wantErr error
	}{
		{
			name:    "wrong pool kind",
			mutate:  func(s *pool.State) { s.Key.Kind = domain.PoolKindV2 },
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "v3 kind rejected",
			mutate:  func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 },
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "unknown token",
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x03},
			amount:  big.NewInt(1),
			wantErr: ErrTokenNotInPool,
		},
		{
			name:    "nil amount",
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  nil,
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "zero amount",
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  new(big.Int),
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "nil liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = nil },
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "zero liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = new(big.Int) },
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrNoLiquidity,
		},
		{
			name:    "negative liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = big.NewInt(-1) },
			adapter: &Adapter{FeePips: 500, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "zero tick spacing",
			adapter: &Adapter{FeePips: 500, TickSpacing: 0},
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: math.ErrInvalidState,
		},
		{
			name:    "fee at denominator",
			adapter: &Adapter{FeePips: maxFeePips, TickSpacing: 10},
			tokenIn: domain.Address{0x01},
			amount:  big.NewInt(1),
			wantErr: math.ErrInvalidState,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := quoteTestState()
			if test.mutate != nil {
				test.mutate(&state)
			}
			_, err := test.adapter.QuoteExactIn(context.Background(), state, pool.DeepState{}, test.tokenIn, test.amount)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestQuoteExactInRespectsCancellation(t *testing.T) {
	state := quoteTestState()
	adapter := &Adapter{FeePips: state.Fee, TickSpacing: state.TickSpacing}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := adapter.QuoteExactIn(ctx, state, pool.DeepState{}, state.Token0, big.NewInt(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
