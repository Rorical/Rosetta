package uniswapv3

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/pool"
)

// TestApplyBurnMissingTicks covers a deep state loaded as a window around the
// current tick, where the boundaries of the burned position may be absent.
func TestApplyBurnMissingTicks(t *testing.T) {
	tests := []struct {
		name          string
		setup         func(t *testing.T) (*pool.State, *pool.DeepState)
		lower         int32
		upper         int32
		amount        int64
		wantLiquidity int64
		check         func(t *testing.T, deep *pool.DeepState)
	}{
		{
			name: "both ticks missing out of range",
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				return &pool.State{Tick: 0, TickSpacing: 60, Liquidity: new(big.Int)}, &pool.DeepState{}
			},
			lower: -120, upper: -60, amount: 500,
			wantLiquidity: 0,
			check: func(t *testing.T, deep *pool.DeepState) {
				if len(deep.Ticks) != 0 {
					t.Fatalf("burn created ticks: %v", deep.Ticks)
				}
				if len(deep.Bitmap) != 0 {
					t.Fatalf("burn set bitmap bits: %v", deep.Bitmap)
				}
			},
		},
		{
			name: "both ticks missing in range",
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				return &pool.State{Tick: 0, TickSpacing: 60, Liquidity: big.NewInt(1000)}, &pool.DeepState{}
			},
			lower: -60, upper: 60, amount: 400,
			wantLiquidity: 600,
			check: func(t *testing.T, deep *pool.DeepState) {
				if len(deep.Ticks) != 0 {
					t.Fatalf("burn created ticks: %v", deep.Ticks)
				}
			},
		},
		{
			name: "lower tick present upper missing in range",
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state := &pool.State{Tick: 0, TickSpacing: 60, Liquidity: big.NewInt(1000)}
				deep := &pool.DeepState{
					Ticks: map[int32]pool.TickInfo{
						-60: {LiquidityGross: big.NewInt(1000), LiquidityNet: big.NewInt(1000)},
					},
				}
				return state, deep
			},
			lower: -60, upper: 60, amount: 400,
			wantLiquidity: 600,
			check: func(t *testing.T, deep *pool.DeepState) {
				assertTick(t, deep, -60, 600, 600)
				if _, ok := deep.Ticks[60]; ok {
					t.Fatalf("burn created missing upper tick")
				}
				if len(deep.Bitmap) != 0 {
					t.Fatalf("burn set bitmap bits for the missing upper tick: %v", deep.Bitmap)
				}
			},
		},
		{
			name: "present tick with zero gross clears its bit",
			setup: func(t *testing.T) (*pool.State, *pool.DeepState) {
				state := &pool.State{Tick: 0, TickSpacing: 60, Liquidity: new(big.Int)}
				deep := &pool.DeepState{}
				if err := Apply(state, deep, Event{
					Kind:      EventMint,
					TickLower: -60,
					TickUpper: 60,
					Amount:    big.NewInt(1000),
				}); err != nil {
					t.Fatalf("mint setup: %v", err)
				}
				return state, deep
			},
			lower: -60, upper: 60, amount: 1000,
			wantLiquidity: 0,
			check: func(t *testing.T, deep *pool.DeepState) {
				for _, tick := range []int32{-60, 60} {
					if bitmapBitSet(deep.Bitmap, tick, 60) {
						t.Fatalf("tick %d bit set after its last position burned", tick)
					}
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state, deep := test.setup(t)
			err := Apply(state, deep, Event{
				Kind:      EventBurn,
				TickLower: test.lower,
				TickUpper: test.upper,
				Amount:    big.NewInt(test.amount),
			})
			if err != nil {
				t.Fatalf("Apply burn: %v", err)
			}
			assertLiquidity(t, state.Liquidity, test.wantLiquidity)
			test.check(t, deep)
		})
	}
}

// TestApplyBurnTicksOutsideLoadedWindow models a position minted while its
// ticks were loaded, followed by a block-level refresh that replaced deep with
// a window excluding both boundaries, then a burn of that position.
func TestApplyBurnTicksOutsideLoadedWindow(t *testing.T) {
	state := &pool.State{Tick: 0, TickSpacing: 60, Liquidity: new(big.Int)}
	deep := &pool.DeepState{}
	if err := Apply(state, deep, Event{
		Kind:      EventMint,
		TickLower: -6000,
		TickUpper: 6000,
		Amount:    big.NewInt(1000),
	}); err != nil {
		t.Fatalf("mint far position: %v", err)
	}
	assertLiquidity(t, state.Liquidity, 1000)

	// The refresh around tick 0 holds neither boundary of the far position.
	windowed := &pool.DeepState{}
	if err := Apply(state, windowed, Event{
		Kind:      EventBurn,
		TickLower: -6000,
		TickUpper: 6000,
		Amount:    big.NewInt(400),
	}); err != nil {
		t.Fatalf("burn outside loaded window: %v", err)
	}
	assertLiquidity(t, state.Liquidity, 600)
	if len(windowed.Ticks) != 0 {
		t.Fatalf("burn outside the window created ticks: %v", windowed.Ticks)
	}

	if err := Apply(state, windowed, Event{
		Kind:      EventBurn,
		TickLower: -6000,
		TickUpper: 6000,
		Amount:    big.NewInt(600),
	}); err != nil {
		t.Fatalf("full burn outside loaded window: %v", err)
	}
	assertLiquidity(t, state.Liquidity, 0)
}
