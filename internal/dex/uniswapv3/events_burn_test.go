package uniswapv3

import (
	"errors"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestApplyBurnInRange(t *testing.T) {
	state, deep := mintedPosition(t, 0, -60, 60, 1000)

	if err := Apply(state, deep, Event{
		Kind:      EventBurn,
		TickLower: -60,
		TickUpper: 60,
		Amount:    big.NewInt(400),
	}); err != nil {
		t.Fatalf("Apply burn: %v", err)
	}
	assertLiquidity(t, state.Liquidity, 600)
	assertTick(t, deep, -60, 600, 600)
	assertTick(t, deep, 60, 600, -600)

	if err := Apply(state, deep, Event{
		Kind:      EventBurn,
		TickLower: -60,
		TickUpper: 60,
		Amount:    big.NewInt(600),
	}); err != nil {
		t.Fatalf("Apply full burn: %v", err)
	}
	assertLiquidity(t, state.Liquidity, 0)
	assertTick(t, deep, -60, 0, 0)
	assertTick(t, deep, 60, 0, 0)
}

func TestApplyBurnOutOfRange(t *testing.T) {
	state, deep := mintedPosition(t, 0, 60, 120, 1000)

	if err := Apply(state, deep, Event{
		Kind:      EventBurn,
		TickLower: 60,
		TickUpper: 120,
		Amount:    big.NewInt(400),
	}); err != nil {
		t.Fatalf("Apply burn: %v", err)
	}
	assertLiquidity(t, state.Liquidity, 0)
	assertTick(t, deep, 60, 600, 600)
	assertTick(t, deep, 120, 600, -600)
}

func TestApplyBurnErrors(t *testing.T) {
	tests := []struct {
		name    string
		lower   int32
		upper   int32
		amount  *big.Int
		setup   func(t *testing.T) (*pool.State, *pool.DeepState)
		wantErr error
	}{
		{
			name: "nil amount", lower: -60, upper: 60, amount: nil,
			setup:   func(t *testing.T) (*pool.State, *pool.DeepState) { return mintedPosition(t, 0, -60, 60, 1000) },
			wantErr: ErrInvalidEvent,
		},
		{
			name: "negative amount", lower: -60, upper: 60, amount: big.NewInt(-1),
			setup:   func(t *testing.T) (*pool.State, *pool.DeepState) { return mintedPosition(t, 0, -60, 60, 1000) },
			wantErr: ErrInvalidEvent,
		},
		{
			name: "equal ticks", lower: 0, upper: 0, amount: big.NewInt(1),
			setup:   emptyPosition,
			wantErr: ErrInvalidEvent,
		},
		{
			name: "gross below burn", lower: -60, upper: 60, amount: big.NewInt(1001),
			setup:   func(t *testing.T) (*pool.State, *pool.DeepState) { return mintedPosition(t, 0, -60, 60, 1000) },
			wantErr: ErrInsufficientLiquidity,
		},
		{
			name:  "present tick gross below burn, other tick missing",
			lower: -60, upper: 60, amount: big.NewInt(1001),
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := mintedPosition(t, 0, -60, 60, 1000)
				delete(deep.Ticks, 60)
				return state, deep
			},
			wantErr: ErrInsufficientLiquidity,
		},
		{
			name:  "present tick has nil gross, other tick missing",
			lower: -60, upper: 60, amount: big.NewInt(1),
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := mintedPosition(t, 0, -60, 60, 1000)
				deep.Ticks[-60] = pool.TickInfo{LiquidityGross: nil, LiquidityNet: big.NewInt(1000)}
				delete(deep.Ticks, 60)
				return state, deep
			},
			wantErr: ErrInvalidEvent,
		},
		{
			name:  "both ticks missing, active liquidity below burn",
			lower: -60, upper: 60, amount: big.NewInt(800),
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := emptyPosition(t)
				state.Liquidity.SetInt64(100)
				return state, deep
			},
			wantErr: ErrInsufficientLiquidity,
		},
		{
			name: "nil gross", lower: -60, upper: 60, amount: big.NewInt(1),
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := mintedPosition(t, 0, -60, 60, 1000)
				deep.Ticks[-60] = pool.TickInfo{LiquidityGross: nil, LiquidityNet: big.NewInt(1000)}
				return state, deep
			},
			wantErr: ErrInvalidEvent,
		},
		{
			name: "nil net", lower: -60, upper: 60, amount: big.NewInt(1),
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := mintedPosition(t, 0, -60, 60, 1000)
				deep.Ticks[60] = pool.TickInfo{LiquidityGross: big.NewInt(1000), LiquidityNet: nil}
				return state, deep
			},
			wantErr: ErrInvalidEvent,
		},
		{
			name: "active liquidity below burn", lower: -60, upper: 60, amount: big.NewInt(800),
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := mintedPosition(t, 0, -60, 60, 1000)
				state.Liquidity.SetInt64(100)
				return state, deep
			},
			wantErr: ErrInsufficientLiquidity,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, deep := test.setup(t)
			err := Apply(state, deep, Event{
				Kind:      EventBurn,
				TickLower: test.lower,
				TickUpper: test.upper,
				Amount:    test.amount,
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestApplyBurnDoesNotMutateOnError(t *testing.T) {
	tests := []struct {
		name  string
		setup func(t *testing.T) (*pool.State, *pool.DeepState)
	}{
		{
			name: "both ticks present",
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := mintedPosition(t, 0, -60, 60, 1000)
				state.Liquidity.SetInt64(100)
				return state, deep
			},
		},
		{
			name: "one tick missing",
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state, deep := mintedPosition(t, 0, -60, 60, 1000)
				state.Liquidity.SetInt64(100)
				delete(deep.Ticks, 60)
				return state, deep
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, deep := test.setup(t)
			beforeState := state.Copy()
			beforeDeep := deep.Copy()

			err := Apply(state, deep, Event{
				Kind:      EventBurn,
				TickLower: -60,
				TickUpper: 60,
				Amount:    big.NewInt(800),
			})
			if !errors.Is(err, ErrInsufficientLiquidity) {
				t.Fatalf("error = %v, want ErrInsufficientLiquidity", err)
			}
			if !reflect.DeepEqual(*state, beforeState) {
				t.Fatalf("failed burn mutated state: %+v, want %+v", *state, beforeState)
			}
			if !reflect.DeepEqual(*deep, beforeDeep) {
				t.Fatalf("failed burn mutated deep: %+v, want %+v", *deep, beforeDeep)
			}
		})
	}
}

func TestApplyBurnClearsBitmapOnlyWhenTickEmpty(t *testing.T) {
	const tickSpacing int32 = 10
	state := &pool.State{Tick: 0, TickSpacing: tickSpacing, Liquidity: new(big.Int)}
	deep := &pool.DeepState{}

	mint := func(lower, upper int32, amount int64) {
		t.Helper()
		if err := Apply(state, deep, Event{
			Kind:      EventMint,
			TickLower: lower,
			TickUpper: upper,
			Amount:    big.NewInt(amount),
		}); err != nil {
			t.Fatalf("mint %d..%d: %v", lower, upper, err)
		}
	}
	burn := func(lower, upper int32, amount int64) {
		t.Helper()
		if err := Apply(state, deep, Event{
			Kind:      EventBurn,
			TickLower: lower,
			TickUpper: upper,
			Amount:    big.NewInt(amount),
		}); err != nil {
			t.Fatalf("burn %d..%d: %v", lower, upper, err)
		}
	}
	mint(-20, 20, 1000)
	mint(-20, 30, 500)

	burn(-20, 20, 400)
	if !bitmapBitSet(deep.Bitmap, 20, tickSpacing) {
		t.Fatalf("partial burn cleared upper tick bit")
	}

	burn(-20, 20, 600)
	if !bitmapBitSet(deep.Bitmap, -20, tickSpacing) {
		t.Fatalf("shared lower tick bit cleared while another position remains")
	}
	if bitmapBitSet(deep.Bitmap, 20, tickSpacing) {
		t.Fatalf("last position at tick 20 did not clear its bit")
	}

	burn(-20, 30, 500)
	for _, tick := range []int32{-20, 30} {
		if bitmapBitSet(deep.Bitmap, tick, tickSpacing) {
			t.Fatalf("tick %d bit set after its last position burned", tick)
		}
	}
}

func TestApplySwap(t *testing.T) {
	state := &pool.State{
		Tick:         1,
		Liquidity:    big.NewInt(5),
		SqrtPriceX96: big.NewInt(6),
	}
	price := new(big.Int).Set(math.Q96)
	liquidity := big.NewInt(12_345)

	err := Apply(state, &pool.DeepState{}, Event{
		Kind:         EventSwap,
		SqrtPriceX96: price,
		Liquidity:    liquidity,
		Tick:         -7,
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if state.Tick != -7 {
		t.Fatalf("tick = %d, want -7", state.Tick)
	}
	if state.SqrtPriceX96.Cmp(math.Q96) != 0 {
		t.Fatalf("sqrt price = %s, want %s", state.SqrtPriceX96, math.Q96)
	}
	assertLiquidity(t, state.Liquidity, 12_345)

	price.SetInt64(1)
	liquidity.SetInt64(1)
	if state.SqrtPriceX96.Cmp(math.Q96) != 0 || state.Liquidity.Int64() != 12_345 {
		t.Fatalf("state shares the event big.Ints")
	}
}

func TestApplySwapErrors(t *testing.T) {
	tests := []struct {
		name string
		ev   Event
	}{
		{"nil sqrt price", Event{Kind: EventSwap, Liquidity: big.NewInt(1)}},
		{"zero sqrt price", Event{Kind: EventSwap, SqrtPriceX96: new(big.Int), Liquidity: big.NewInt(1)}},
		{"negative sqrt price", Event{Kind: EventSwap, SqrtPriceX96: big.NewInt(-1), Liquidity: big.NewInt(1)}},
		{"nil liquidity", Event{Kind: EventSwap, SqrtPriceX96: big.NewInt(1)}},
		{"negative liquidity", Event{Kind: EventSwap, SqrtPriceX96: big.NewInt(1), Liquidity: big.NewInt(-1)}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := Apply(&pool.State{}, &pool.DeepState{}, test.ev); !errors.Is(err, ErrInvalidEvent) {
				t.Fatalf("error = %v, want ErrInvalidEvent", err)
			}
		})
	}
}

func TestApplyCollectIsNoOp(t *testing.T) {
	state := &pool.State{
		Tick:         -3,
		Liquidity:    big.NewInt(7),
		SqrtPriceX96: big.NewInt(8),
	}
	deep := &pool.DeepState{}
	before := state.Copy()

	err := Apply(state, deep, Event{
		Kind:      EventCollect,
		TickLower: -60,
		TickUpper: 60,
		Amount:    big.NewInt(100),
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if state.Tick != before.Tick || state.Key != before.Key {
		t.Fatalf("collect changed scalar state")
	}
	if state.Liquidity.Cmp(before.Liquidity) != 0 || state.SqrtPriceX96.Cmp(before.SqrtPriceX96) != 0 {
		t.Fatalf("collect changed big.Int state")
	}
	if deep.Bitmap != nil || deep.Ticks != nil {
		t.Fatalf("collect initialized deep maps")
	}
}

func TestApplyRejectsNilStateOrUnknownKind(t *testing.T) {
	state := &pool.State{}
	deep := &pool.DeepState{}

	if err := Apply(nil, deep, Event{Kind: EventCollect}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("nil state error = %v, want ErrInvalidEvent", err)
	}
	if err := Apply(state, nil, Event{Kind: EventCollect}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("nil deep error = %v, want ErrInvalidEvent", err)
	}
	if err := Apply(state, deep, Event{Kind: EventKind(0)}); !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("unknown kind error = %v, want ErrInvalidEvent", err)
	}
}
