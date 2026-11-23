package math

import (
	"math/big"
	"testing"
)

func TestSwapV3ResultLiquidity(t *testing.T) {
	t.Run("no crossing", func(t *testing.T) {
		liquidity := big.NewInt(1_000_000_000_000_000_000)
		state := V3State{
			SqrtPriceX96: new(big.Int).Set(Q96),
			Tick:         0,
			Liquidity:    liquidity,
			TickSpacing:  60,
			Bitmap:       map[int16]*big.Int{},
			Ticks:        map[int32]TickInfo{},
		}

		result, err := SwapV3ExactIn(state, false, big.NewInt(1_000_000_000_000_000_000))
		if err != nil {
			t.Fatalf("SwapV3ExactIn: %v", err)
		}
		if result.Liquidity == nil {
			t.Fatal("result liquidity is nil")
		}
		if result.Liquidity.Cmp(liquidity) != 0 {
			t.Fatalf("result liquidity = %s, want %s", result.Liquidity, liquidity)
		}
		if result.Liquidity == state.Liquidity {
			t.Fatal("result liquidity aliases the pre-state")
		}

		result.Liquidity.SetInt64(7)
		if state.Liquidity.Cmp(liquidity) != 0 {
			t.Fatalf("pre-state liquidity changed to %s", state.Liquidity)
		}
	})

	t.Run("crossing", func(t *testing.T) {
		state := crossingState()
		preLiquidity := new(big.Int).Set(state.Liquidity)

		result, err := SwapV3ExactIn(state, true, big.NewInt(2_000_000_000_000_000))
		if err != nil {
			t.Fatalf("SwapV3ExactIn: %v", err)
		}
		if result.Liquidity == nil {
			t.Fatal("result liquidity is nil")
		}
		want := new(big.Int).Mul(preLiquidity, big.NewInt(2))
		if result.Liquidity.Cmp(want) != 0 {
			t.Fatalf("result liquidity = %s, want %s after crossing two ticks", result.Liquidity, want)
		}
		if result.Liquidity == state.Liquidity {
			t.Fatal("result liquidity aliases the pre-state")
		}

		result.Liquidity.SetInt64(7)
		if state.Liquidity.Cmp(preLiquidity) != 0 {
			t.Fatalf("pre-state liquidity changed to %s", state.Liquidity)
		}
	})
}
