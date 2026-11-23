package uniswapv2

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
		name    string
		feePips uint32
		wantErr bool
	}{
		{"zero fee", 0, false},
		{"standard fee", 3000, false},
		{"max fee", 999_999, false},
		{"fee at denominator", 1_000_000, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			adapter, err := New(test.feePips)
			if test.wantErr {
				if !errors.Is(err, ErrInvalidConfig) {
					t.Fatalf("error = %v, want ErrInvalidConfig", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if adapter.feePips != test.feePips {
				t.Fatalf("fee = %d, want %d", adapter.feePips, test.feePips)
			}
			if adapter.Kind() != domain.PoolKindV2 {
				t.Fatalf("Kind = %d, want V2", adapter.Kind())
			}
		})
	}
}

func TestQuoteBothDirections(t *testing.T) {
	state := quoteTestState()
	adapter, err := New(0)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	tests := []struct {
		name    string
		tokenIn domain.Address
		want    int64
	}{
		{"token0 in", testToken0, 181_818_181},
		{"token1 in", testToken1, 47_619_047},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := adapter.QuoteExactIn(context.Background(), state, pool.DeepState{}, test.tokenIn, big.NewInt(100_000_000))
			if err != nil {
				t.Fatalf("QuoteExactIn: %v", err)
			}
			if got.Cmp(big.NewInt(test.want)) != 0 {
				t.Fatalf("quote = %s, want %d", got, test.want)
			}
		})
	}
}

func TestQuoteFeeReducesOutput(t *testing.T) {
	state := quoteTestState()
	tests := []struct {
		name    string
		tokenIn domain.Address
		want    int64
	}{
		{"token0 in", testToken0, 181_322_178},
		{"token1 in", testToken1, 47_482_973},
	}
	adapter, err := New(3000)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := adapter.QuoteExactIn(context.Background(), state, pool.DeepState{}, test.tokenIn, big.NewInt(100_000_000))
			if err != nil {
				t.Fatalf("QuoteExactIn: %v", err)
			}
			if got.Cmp(big.NewInt(test.want)) != 0 {
				t.Fatalf("quote = %s, want %d", got, test.want)
			}
			reserveIn, reserveOut := state.Reserve0, state.Reserve1
			if test.tokenIn == testToken1 {
				reserveIn, reserveOut = reserveOut, reserveIn
			}
			feeFree, err := math.QuoteV2ExactIn(reserveIn, reserveOut, 0, big.NewInt(100_000_000))
			if err != nil {
				t.Fatalf("QuoteV2ExactIn: %v", err)
			}
			if got.Cmp(feeFree) >= 0 {
				t.Fatalf("fee quote %s is not below fee-free quote %s", got, feeFree)
			}
		})
	}
}

func TestQuoteValidation(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*pool.State)
		ctx     func() context.Context
		tokenIn domain.Address
		amount  *big.Int
		wantErr error
	}{
		{
			name:    "wrong pool kind",
			mutate:  func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 },
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
			name:    "zero reserve0",
			mutate:  func(s *pool.State) { s.Reserve0 = new(big.Int) },
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrNoLiquidity,
		},
		{
			name:    "zero reserve1",
			mutate:  func(s *pool.State) { s.Reserve1 = new(big.Int) },
			tokenIn: testToken1,
			amount:  big.NewInt(1),
			wantErr: pool.ErrNoLiquidity,
		},
		{
			name:    "negative reserve",
			mutate:  func(s *pool.State) { s.Reserve0 = big.NewInt(-1) },
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrNoLiquidity,
		},
		{
			name:    "nil reserve0",
			mutate:  func(s *pool.State) { s.Reserve0 = nil },
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "nil reserve1",
			mutate:  func(s *pool.State) { s.Reserve1 = nil },
			tokenIn: testToken1,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
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
			name:    "canceled context",
			ctx:     canceledContext,
			tokenIn: testToken0,
			amount:  big.NewInt(1),
			wantErr: context.Canceled,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := quoteTestState()
			if test.mutate != nil {
				test.mutate(&state)
			}
			ctx := context.Background()
			if test.ctx != nil {
				ctx = test.ctx()
			}
			adapter, err := New(3000)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = adapter.QuoteExactIn(ctx, state, pool.DeepState{}, test.tokenIn, test.amount)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func canceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func quoteTestState() pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: domain.Address{0x09},
			Kind:    domain.PoolKindV2,
		},
		Token0:   testToken0,
		Token1:   testToken1,
		Fee:      3000,
		Reserve0: big.NewInt(1_000_000_000),
		Reserve1: big.NewInt(2_000_000_000),
	}
}
