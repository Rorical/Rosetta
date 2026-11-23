package math

import (
	"errors"
	"math/big"
	"testing"
)

// TestSwapV3Window checks the loaded-window guard: a quote may not leave the
// tick range the caller loaded. A zero/zero window means the range is unknown
// (an old snapshot or event-replayed state), so enforcement is skipped.
func TestSwapV3Window(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	base := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    liquidity,
		FeePips:      0,
		TickSpacing:  60,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}

	tests := []struct {
		name       string
		zeroForOne bool
		windowMin  int32
		windowMax  int32
		wantErr    error
	}{
		{name: "in window zero for one", zeroForOne: true, windowMin: -20000, windowMax: 20000},
		{name: "in window one for zero", zeroForOne: false, windowMin: -20000, windowMax: 20000},
		{name: "outside window zero for one", zeroForOne: true, windowMin: -256, windowMax: 255, wantErr: ErrOutOfWindow},
		{name: "outside window one for zero", zeroForOne: false, windowMin: -256, windowMax: 255, wantErr: ErrOutOfWindow},
		{name: "unknown window is not enforced", zeroForOne: true, windowMin: 0, windowMax: 0},
		{name: "reversed window", zeroForOne: true, windowMin: 255, windowMax: -256, wantErr: ErrInvalidState},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := base
			state.WindowMin = test.windowMin
			state.WindowMax = test.windowMax

			result, err := SwapV3ExactIn(state, test.zeroForOne, liquidity)
			if test.wantErr != nil {
				if !errors.Is(err, test.wantErr) {
					t.Fatalf("error = %v, want %v", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("SwapV3ExactIn: %v", err)
			}
			if result.AmountOut.Sign() <= 0 {
				t.Fatalf("amount out = %s, want positive", result.AmountOut)
			}
			if (test.windowMin != 0 || test.windowMax != 0) &&
				(result.Tick < test.windowMin || result.Tick > test.windowMax) {
				t.Fatalf("final tick = %d, outside [%d, %d]", result.Tick, test.windowMin, test.windowMax)
			}
		})
	}
}

// The exit side decides the class: a side that covered CoverageTicks is a
// capacity limit, a narrower side still needs ticks.
func TestCheckWindowCoverageBoundary(t *testing.T) {
	tests := []struct {
		name       string
		state      V3State
		zeroForOne bool
		tick       int32
		want       error
	}{
		{"inside", V3State{Tick: 100, WindowMin: -5000, WindowMax: 5000}, true, -4000, nil},
		{"unknown window", V3State{Tick: 100}, true, -900000, nil},
		{"down side covered", V3State{Tick: 100, WindowMin: 100 - CoverageTicks, WindowMax: 200}, true, -5000, ErrCapacity},
		{"down side short", V3State{Tick: 100, WindowMin: 101 - CoverageTicks, WindowMax: 90000}, true, -5000, ErrOutOfWindow},
		{"up side covered", V3State{Tick: 100, WindowMin: -1, WindowMax: 100 + CoverageTicks}, false, 9000, ErrCapacity},
		{"up side short", V3State{Tick: 100, WindowMin: -90000, WindowMax: 99 + CoverageTicks}, false, 9000, ErrOutOfWindow},
		{"start outside", V3State{Tick: 9000, WindowMin: -5000, WindowMax: 5000}, true, -6000, ErrOutOfWindow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := checkWindow(test.state, test.zeroForOne, test.tick)
			if test.want == nil && err != nil || test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("got %v, want %v", err, test.want)
			}
		})
	}
}
