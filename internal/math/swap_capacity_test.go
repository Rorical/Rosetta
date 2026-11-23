package math_test

import (
	"errors"
	"math/big"
	"os"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/state"
)

// capturedWindowState loads one pool from a canonical OP snapshot whose tick
// windows were maintained from logs and widened by the runtime's repair loop.
func capturedWindowState(t *testing.T, label string) math.V3State {
	t.Helper()
	raw, err := os.ReadFile("testdata/op-windows-158960149.json")
	if err != nil {
		t.Fatal(err)
	}
	store := state.New()
	if err := store.LoadSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	for _, p := range store.Pools() {
		if !strings.HasPrefix(strings.ToLower(p.Key.Address.Hex()), label) && !strings.HasPrefix(strings.ToLower(p.Key.PoolID.Hex()), label) {
			continue
		}
		deep, ok := store.Deep(p.Key)
		if !ok {
			t.Fatalf("pool %s has no deep state", label)
		}
		return math.V3State{SqrtPriceX96: p.SqrtPriceX96, Tick: p.Tick, Liquidity: p.Liquidity, FeePips: p.Fee, TickSpacing: p.TickSpacing,
			Bitmap: deep.Bitmap, Ticks: deep.Ticks, WindowMin: deep.WindowMin, WindowMax: deep.WindowMax}
	}
	t.Fatalf("pool %s missing from fixture", label)
	return math.V3State{}
}

func amount(t *testing.T, text string) *big.Int {
	t.Helper()
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		t.Fatal("bad amount", text)
	}
	return value
}

// The wstETH/WETH Slipstream pool (spacing 1) had already been widened to the
// repair loop's 64-word cap: 129 words, [-14336, 18687]. A sizing sample of
// 1000 WETH moves the price far past every loaded word. Before the capacity
// classification this was ErrOutOfWindow, so every frame requested another
// repair that could never cover the final tick.
func TestCapturedSwapPastCoverageIsCapacity(t *testing.T) {
	slipstream := capturedWindowState(t, "0xbf30ff33")
	for _, test := range []struct {
		name       string
		zeroForOne bool
		input      string
	}{
		{"weth in", false, "1000000000000000000000"},
		{"wsteth in", true, "1000000000000000000000"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := math.QuoteV3ExactIn(slipstream, test.zeroForOne, amount(t, test.input))
			if !errors.Is(err, math.ErrCapacity) || errors.Is(err, math.ErrOutOfWindow) {
				t.Fatalf("swap past a %d-tick covered window: %v", math.CoverageTicks, err)
			}
		})
	}
}

// Liquidity above this price is thin: 0.01 WETH already leaves the window,
// while 0.0001 WETH stays inside it and matches the swap replay exactly.
func TestCapturedSwapInsideWindowIsExact(t *testing.T) {
	slipstream := capturedWindowState(t, "0xbf30ff33")
	input := amount(t, "100000000000000")
	want, err := math.SwapV3ExactIn(slipstream, false, input)
	if err != nil {
		t.Fatal(err)
	}
	got, err := math.QuoteV3ExactIn(slipstream, false, input)
	if err != nil || got.Cmp(want.AmountOut) != 0 {
		t.Fatalf("quote %v %v, swap %v", got, err, want.AmountOut)
	}
}

// The thin V4 pool at tick 0 holds a five-word window: 767 ticks above the
// price, under the coverage target. Leaving it still needs more ticks, so it
// stays a repairable window error rather than a capacity limit.
func TestCapturedSwapPastNarrowWindowNeedsTicks(t *testing.T) {
	v4 := capturedWindowState(t, "0x0535e61d")
	_, err := math.QuoteV3ExactIn(v4, false, amount(t, "1000000000000000"))
	if !errors.Is(err, math.ErrOutOfWindow) || errors.Is(err, math.ErrCapacity) {
		t.Fatalf("narrow window exit: %v", err)
	}
}

// When the loaded window reaches the price bound, a swap that drains every
// position stops at the bound with input left over. The executor requires the
// whole input (SwapExecutor.swapV3 and V4SwapExecutor revert otherwise), so a
// quote is a capacity limit while the explicit-limit replay keeps HitLimit.
func TestDrainedFullRangeSwapIsCapacity(t *testing.T) {
	bitmap := map[int16]*big.Int{}
	ticks := map[int32]math.TickInfo{}
	liquidity := big.NewInt(1_000_000_000_000)
	for _, tick := range []int32{-600, 600} {
		compressed := tick / 60
		word, bit := int16(compressed>>8), uint(compressed&0xff)
		if bitmap[word] == nil {
			bitmap[word] = new(big.Int)
		}
		bitmap[word].SetBit(bitmap[word], int(bit), 1)
	}
	ticks[-600] = math.TickInfo{LiquidityGross: liquidity, LiquidityNet: liquidity}
	ticks[600] = math.TickInfo{LiquidityGross: liquidity, LiquidityNet: new(big.Int).Neg(liquidity)}
	state := math.V3State{SqrtPriceX96: new(big.Int).Set(math.Q96), Liquidity: liquidity, FeePips: 3000, TickSpacing: 60,
		Bitmap: bitmap, Ticks: ticks, WindowMin: math.MinTick, WindowMax: math.MaxTick}
	input := big.NewInt(1_000_000_000_000)
	replay, err := math.SwapV3ExactIn(state, true, input)
	if err != nil || !replay.HitLimit || replay.AmountInUsed.Cmp(input) >= 0 {
		t.Fatalf("replay %+v %v", replay, err)
	}
	if _, err := math.QuoteV3ExactIn(state, true, input); !errors.Is(err, math.ErrCapacity) {
		t.Fatalf("drained quote: %v", err)
	}
	filled, err := math.QuoteV3ExactIn(state, true, new(big.Int).Sub(replay.AmountInUsed, big.NewInt(1)))
	if err != nil || filled.Sign() <= 0 {
		t.Fatalf("input below capacity: %v %v", filled, err)
	}
}
