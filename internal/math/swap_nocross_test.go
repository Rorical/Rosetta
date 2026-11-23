package math

import (
	"math/big"
	"testing"
)

func TestSwapV3NoCrossConstantProduct(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	state := V3State{
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
		amountIn   *big.Int
		wantOut    *big.Int
		wantPrice  *big.Int
	}{
		{
			name:       "zero for one",
			zeroForOne: true,
			amountIn:   new(big.Int).Set(liquidity),
			wantOut:    new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(2)),
			wantPrice:  new(big.Int).Div(new(big.Int).Set(Q96), big.NewInt(2)),
		},
		{
			name:       "one for zero",
			zeroForOne: false,
			amountIn:   new(big.Int).Set(liquidity),
			wantOut:    new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(2)),
			wantPrice:  new(big.Int).Mul(Q96, big.NewInt(2)),
		},
		{
			name:       "fractional input",
			zeroForOne: true,
			amountIn:   new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(4)),
			wantOut:    new(big.Int).Sub(new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(5)), big.NewInt(1)),
			wantPrice:  new(big.Int).Div(new(big.Int).Add(new(big.Int).Mul(new(big.Int).Set(Q96), big.NewInt(4)), big.NewInt(1)), big.NewInt(5)),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			amountBefore := new(big.Int).Set(test.amountIn)
			result, err := SwapV3ExactIn(state, test.zeroForOne, test.amountIn)
			if err != nil {
				t.Fatalf("SwapV3ExactIn: %v", err)
			}
			if result.AmountOut.Cmp(test.wantOut) != 0 {
				t.Fatalf("amount out = %s, want %s", result.AmountOut, test.wantOut)
			}
			if result.AmountInUsed.Cmp(test.amountIn) != 0 {
				t.Fatalf("amount in used = %s, want %s", result.AmountInUsed, test.amountIn)
			}
			if result.SqrtPriceX96.Cmp(test.wantPrice) != 0 {
				t.Fatalf("sqrt price = %s, want %s", result.SqrtPriceX96, test.wantPrice)
			}
			if len(result.Crossed) != 0 {
				t.Fatalf("crossed ticks = %v, want none", result.Crossed)
			}
			if result.HitLimit {
				t.Fatalf("unexpected price limit")
			}
			if test.amountIn.Cmp(amountBefore) != 0 {
				t.Fatalf("swap mutated amountIn")
			}
			if state.SqrtPriceX96.Cmp(Q96) != 0 || state.Liquidity.Cmp(liquidity) != 0 {
				t.Fatalf("swap mutated state")
			}
		})
	}
}

func TestSwapV3FeeAppliedToInput(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    liquidity,
		FeePips:      3000,
		TickSpacing:  60,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}
	amountIn := new(big.Int).Set(liquidity)
	result, err := SwapV3ExactIn(state, true, amountIn)
	if err != nil {
		t.Fatalf("SwapV3ExactIn: %v", err)
	}
	if result.AmountInUsed.Cmp(amountIn) != 0 {
		t.Fatalf("amount in used = %s, want %s", result.AmountInUsed, amountIn)
	}

	effective := new(big.Rat).Mul(
		new(big.Rat).SetInt(amountIn),
		new(big.Rat).SetFrac(big.NewInt(feePipsDenominator-3000), big.NewInt(feePipsDenominator)),
	)
	expected := new(big.Rat).Mul(new(big.Rat).SetInt(liquidity), effective)
	expected.Quo(expected, new(big.Rat).Add(new(big.Rat).SetInt(liquidity), effective))
	if delta := new(big.Rat).Sub(new(big.Rat).SetInt(result.AmountOut), expected); ratAbs(delta).Cmp(big.NewRat(2, 1)) > 0 {
		t.Fatalf("amount out = %s, want within 2 of %s", result.AmountOut, expected.FloatString(6))
	}
}

func TestSwapV3ZeroFeeUsesFullInput(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    liquidity,
		TickSpacing:  10,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}
	amountIn := big.NewInt(123_456_789_012_345_678)
	result, err := SwapV3ExactIn(state, true, amountIn)
	if err != nil {
		t.Fatalf("SwapV3ExactIn: %v", err)
	}
	if result.AmountInUsed.Cmp(amountIn) != 0 {
		t.Fatalf("amount in used = %s, want %s", result.AmountInUsed, amountIn)
	}
}

func TestSwapV3InvalidInputs(t *testing.T) {
	valid := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Liquidity:    big.NewInt(1_000_000_000_000_000_000),
		TickSpacing:  10,
	}

	tests := []struct {
		name     string
		state    V3State
		amountIn *big.Int
	}{
		{"nil amount", valid, nil},
		{"zero amount", valid, new(big.Int)},
		{"negative amount", valid, big.NewInt(-1)},
		{"zero liquidity", withLiquidity(valid, new(big.Int)), big.NewInt(1)},
		{"negative liquidity", withLiquidity(valid, big.NewInt(-1)), big.NewInt(1)},
		{"negative spacing", withSpacing(valid, -1), big.NewInt(1)},
		{"full fee", withFee(valid, feePipsDenominator), big.NewInt(1)},
		{"tick out of range", withTick(valid, MaxTick+1), big.NewInt(1)},
		{"tick inconsistent with price", withTick(valid, 10), big.NewInt(1)},
		{"price out of range", withPrice(valid, new(big.Int)), big.NewInt(1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SwapV3ExactIn(test.state, true, test.amountIn); err == nil {
				t.Fatalf("SwapV3ExactIn returned no error")
			}
		})
	}
}

func withLiquidity(state V3State, liquidity *big.Int) V3State {
	state.Liquidity = liquidity
	return state
}

func withSpacing(state V3State, spacing int32) V3State {
	state.TickSpacing = spacing
	return state
}

func withFee(state V3State, fee uint32) V3State {
	state.FeePips = fee
	return state
}

func withTick(state V3State, tick int32) V3State {
	state.Tick = tick
	return state
}

func withPrice(state V3State, price *big.Int) V3State {
	state.SqrtPriceX96 = price
	return state
}
