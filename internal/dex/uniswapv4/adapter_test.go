package uniswapv4

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

var (
	quoteToken0 = domain.Address{0x01}
	quoteToken1 = domain.Address{0x02}
)

func TestNewAdapter(t *testing.T) {
	adapter, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if adapter.Kind() != domain.PoolKindV4 {
		t.Fatalf("Kind = %d, want V4", adapter.Kind())
	}
}

func TestQuoteExactInUsesEffectiveFee(t *testing.T) {
	state := quoteState()
	deep := quoteDeep()
	amountIn := big.NewInt(50_000_000)
	adapter, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name       string
		tokenIn    domain.Address
		zeroForOne bool
	}{
		{"token0 in", quoteToken0, true},
		{"token1 in", quoteToken1, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := adapter.QuoteExactIn(context.Background(), state, deep, test.tokenIn, amountIn)
			if err != nil {
				t.Fatalf("QuoteExactIn: %v", err)
			}
			// 500 lp + 125 protocol - 500*125/1e6 = 625 pips.
			want := v3Quote(t, state, 625, deep, test.tokenIn, amountIn)
			if got.Cmp(want) != 0 {
				t.Fatalf("quote = %s, want V3 quote at 625 pips %s", got, want)
			}
		})
	}
}

func TestQuoteExactInSelectsProtocolFeeHalf(t *testing.T) {
	state := quoteState()
	state.ProtocolFee = 125 | 500<<12
	deep := quoteDeep()
	amountIn := big.NewInt(50_000_000)
	adapter, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	zeroForOne, err := adapter.QuoteExactIn(context.Background(), state, deep, quoteToken0, amountIn)
	if err != nil {
		t.Fatalf("QuoteExactIn token0: %v", err)
	}
	// Low half: 500 lp + 125 protocol = 625 pips.
	wantZeroForOne := v3Quote(t, state, 625, deep, quoteToken0, amountIn)
	if zeroForOne.Cmp(wantZeroForOne) != 0 {
		t.Fatalf("token0 quote = %s, want %s", zeroForOne, wantZeroForOne)
	}

	oneForZero, err := adapter.QuoteExactIn(context.Background(), state, deep, quoteToken1, amountIn)
	if err != nil {
		t.Fatalf("QuoteExactIn token1: %v", err)
	}
	// High half: 500 lp + 500 protocol = 1000 pips.
	wantOneForZero := v3Quote(t, state, 1000, deep, quoteToken1, amountIn)
	if oneForZero.Cmp(wantOneForZero) != 0 {
		t.Fatalf("token1 quote = %s, want %s", oneForZero, wantOneForZero)
	}
	if zeroForOne.Cmp(oneForZero) == 0 {
		t.Fatal("directions produced the same quote; the protocol fee half was ignored")
	}
}

func TestQuoteExactInValidation(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*pool.State)
		nilBitmap bool
		nilTicks  bool
		tokenIn   domain.Address
		amount    *big.Int
		wantErr   error
	}{
		{
			name:    "wrong pool kind",
			mutate:  func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "hooks are unsupported",
			mutate:  func(s *pool.State) { s.Key.Hooks = domain.Address{0x0f} },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: ErrUnsupportedHooks,
		},
		{
			name:    "zero pool ID",
			mutate:  func(s *pool.State) { s.Key.PoolID = domain.Hash{} },
			tokenIn: quoteToken0,
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
			name:    "nil liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = nil },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:    "zero liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = new(big.Int) },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrNoLiquidity,
		},
		{
			name:    "negative liquidity",
			mutate:  func(s *pool.State) { s.Liquidity = big.NewInt(-1) },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: pool.ErrInvalidState,
		},
		{
			name:      "missing bitmap",
			nilBitmap: true,
			tokenIn:   quoteToken0,
			amount:    big.NewInt(1),
			wantErr:   ErrMissingTickState,
		},
		{
			name:     "missing ticks",
			nilTicks: true,
			tokenIn:  quoteToken0,
			amount:   big.NewInt(1),
			wantErr:  ErrMissingTickState,
		},
		{
			name:    "dynamic fee flag",
			mutate:  func(s *pool.State) { s.Fee = 0x800000 },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: ErrInvalidFee,
		},
		{
			name:    "lp fee at denominator",
			mutate:  func(s *pool.State) { s.Fee = 1_000_000 },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: ErrInvalidFee,
		},
		{
			name:    "protocol fee above two halves",
			mutate:  func(s *pool.State) { s.ProtocolFee = 0x1000000 },
			tokenIn: quoteToken0,
			amount:  big.NewInt(1),
			wantErr: ErrInvalidFee,
		},
		{
			name:    "nil amount",
			tokenIn: quoteToken0,
			amount:  nil,
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "zero amount",
			tokenIn: quoteToken0,
			amount:  new(big.Int),
			wantErr: math.ErrInvalidArgument,
		},
		{
			name:    "negative amount",
			tokenIn: quoteToken0,
			amount:  big.NewInt(-1),
			wantErr: math.ErrInvalidArgument,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := quoteState()
			if test.mutate != nil {
				test.mutate(&state)
			}
			deep := quoteDeep()
			if test.nilBitmap {
				deep.Bitmap = nil
			}
			if test.nilTicks {
				deep.Ticks = nil
			}
			adapter, err := New()
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = adapter.QuoteExactIn(context.Background(), state, deep, test.tokenIn, test.amount)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestQuoteExactInRespectsCancellation(t *testing.T) {
	adapter, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := adapter.QuoteExactIn(ctx, quoteState(), quoteDeep(), quoteToken0, big.NewInt(1)); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func quoteState() pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: poolManagerAddress,
			Kind:    domain.PoolKindV4,
			PoolID:  testPoolID,
		},
		Token0:       quoteToken0,
		Token1:       quoteToken1,
		Fee:          500,
		ProtocolFee:  0x07d07d,
		TickSpacing:  10,
		SqrtPriceX96: q96(),
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000_000),
	}
}

func quoteDeep() pool.DeepState {
	return pool.DeepState{
		Bitmap: map[int16]*big.Int{},
		Ticks:  map[int32]pool.TickInfo{},
	}
}

// v3Quote prices the same state through the V3 adapter at feePips. The pool
// shape and tick state are V3-shaped, so a V4 quote must agree exactly.
func v3Quote(t *testing.T, state pool.State, feePips uint32, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) *big.Int {
	t.Helper()
	v3State := state
	v3State.Key.Kind = domain.PoolKindV3
	v3State.ProtocolFee = 0
	adapter, err := uniswapv3.New(feePips, state.TickSpacing)
	if err != nil {
		t.Fatalf("uniswapv3.New: %v", err)
	}
	out, err := adapter.QuoteExactIn(context.Background(), v3State, deep, tokenIn, amountIn)
	if err != nil {
		t.Fatalf("V3 quote: %v", err)
	}
	return out
}
