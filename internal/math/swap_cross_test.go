package math

import (
	"errors"
	"math/big"
	"slices"
	"testing"
)

func TestSwapV3CrossingOneTickIsContinuous(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	bitmap := map[int16]*big.Int{0: wordWithBits(1)}
	ticks := map[int32]TickInfo{
		10: {
			LiquidityGross: new(big.Int).Set(liquidity),
			LiquidityNet:   new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(2)),
		},
	}
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    liquidity,
		TickSpacing:  10,
		Bitmap:       bitmap,
		Ticks:        ticks,
	}

	sqrt10 := sqrtAt(t, 10)
	toTick, err := amount1Delta(Q96, sqrt10, liquidity, true)
	if err != nil {
		t.Fatalf("amount1Delta: %v", err)
	}
	delta := big.NewInt(1_000_000_000_000)

	below, err := SwapV3ExactIn(state, false, new(big.Int).Sub(toTick, delta))
	if err != nil {
		t.Fatalf("below swap: %v", err)
	}
	above, err := SwapV3ExactIn(state, false, new(big.Int).Add(toTick, delta))
	if err != nil {
		t.Fatalf("above swap: %v", err)
	}
	if len(below.Crossed) != 0 {
		t.Fatalf("below crossed = %v, want none", below.Crossed)
	}
	if !slices.Equal(above.Crossed, []int32{10}) {
		t.Fatalf("above crossed = %v, want [10]", above.Crossed)
	}

	price := new(big.Rat).SetFrac(Q96, sqrt10)
	price.Mul(price, price)
	expected := new(big.Rat).Mul(price, new(big.Rat).SetInt(new(big.Int).Lsh(delta, 1)))
	actual := new(big.Int).Sub(above.AmountOut, below.AmountOut)
	if actual.Sign() <= 0 {
		t.Fatalf("output did not increase across the tick: %s", actual)
	}
	deltaOutput := new(big.Rat).Sub(new(big.Rat).SetInt(actual), expected)
	if ratAbs(deltaOutput).Cmp(new(big.Rat).SetInt64(1_000_000)) > 0 {
		t.Fatalf("output jump = %s, want near %s", actual, expected.FloatString(0))
	}
}

func TestSwapV3CrossingSeveralTicks(t *testing.T) {
	state := crossingState()
	result, err := SwapV3ExactIn(state, true, big.NewInt(2_000_000_000_000_000))
	if err != nil {
		t.Fatalf("SwapV3ExactIn: %v", err)
	}
	if !slices.Equal(result.Crossed, []int32{-10, -20}) {
		t.Fatalf("crossed = %v, want [-10 -20]", result.Crossed)
	}
	if result.Tick != -33 {
		t.Fatalf("final tick = %d, want -33", result.Tick)
	}

	liquidity := big.NewInt(1_000_000_000_000_000_000)
	sqrt10 := sqrtAt(t, -10)
	sqrt20 := sqrtAt(t, -20)
	sqrt40 := sqrtAt(t, -40)
	remaining := new(big.Rat).SetInt(big.NewInt(2_000_000_000_000_000))
	total := new(big.Rat)
	segments := []struct {
		liquidity *big.Int
		sqrtHigh  *big.Int
		sqrtLow   *big.Int
	}{
		{liquidity, new(big.Int).Set(Q96), sqrt10},
		{new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(2)), sqrt10, sqrt20},
		{new(big.Int).Mul(new(big.Int).Set(liquidity), big.NewInt(2)), sqrt20, sqrt40},
	}
	for _, segment := range segments {
		out, left := segmentZeroForOneOut(segment.liquidity, segment.sqrtHigh, segment.sqrtLow, remaining)
		total.Add(total, out)
		remaining = left
		if remaining.Sign() == 0 {
			break
		}
	}
	delta := new(big.Rat).Sub(new(big.Rat).SetInt(result.AmountOut), total)
	if ratAbs(delta).Cmp(new(big.Rat).SetInt64(10)) > 0 {
		t.Fatalf("amount out = %s, want within 10 of %s", result.AmountOut, total.FloatString(6))
	}
}

func TestSwapV3CrossingZeroForOneIsContinuous(t *testing.T) {
	state := crossingState()
	liquidity := new(big.Int).Set(state.Liquidity)
	sqrt10 := sqrtAt(t, -10)
	toTick, err := amount0Delta(sqrt10, Q96, liquidity, true)
	if err != nil {
		t.Fatalf("amount0Delta: %v", err)
	}
	delta := big.NewInt(1_000_000_000_000)
	below, err := SwapV3ExactIn(state, true, new(big.Int).Sub(toTick, delta))
	if err != nil {
		t.Fatalf("below swap: %v", err)
	}
	above, err := SwapV3ExactIn(state, true, new(big.Int).Add(toTick, delta))
	if err != nil {
		t.Fatalf("above swap: %v", err)
	}
	if len(below.Crossed) != 0 {
		t.Fatalf("below crossed = %v, want none", below.Crossed)
	}
	if !slices.Equal(above.Crossed, []int32{-10}) {
		t.Fatalf("above crossed = %v, want [-10]", above.Crossed)
	}

	price := new(big.Rat).SetFrac(sqrt10, Q96)
	price.Mul(price, price)
	expected := new(big.Rat).Mul(price, new(big.Rat).SetInt(new(big.Int).Lsh(delta, 1)))
	actual := new(big.Int).Sub(above.AmountOut, below.AmountOut)
	if actual.Sign() <= 0 {
		t.Fatalf("output did not increase across the tick: %s", actual)
	}
	deltaOutput := new(big.Rat).Sub(new(big.Rat).SetInt(actual), expected)
	if ratAbs(deltaOutput).Cmp(new(big.Rat).SetInt64(1_000_000)) > 0 {
		t.Fatalf("output jump = %s, want near %s", actual, expected.FloatString(0))
	}
}

func TestSwapV3MissingTickDataIsError(t *testing.T) {
	state := crossingState()
	delete(state.Ticks, -10)
	_, err := SwapV3ExactIn(state, true, big.NewInt(1_000_000_000_000_000))
	if !errors.Is(err, ErrMissingTick) {
		t.Fatalf("error = %v, want ErrMissingTick", err)
	}
}

func TestSwapV3StartingOnInitializedTick(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	half := new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(2))
	bitmap := map[int16]*big.Int{
		-1: wordWithBits(255, 254),
	}
	ticks := map[int32]TickInfo{
		-10: {LiquidityGross: new(big.Int).Mul(half, big.NewInt(3)), LiquidityNet: half},
		-20: {LiquidityGross: half, LiquidityNet: half},
		10:  {LiquidityGross: liquidity, LiquidityNet: new(big.Int).Neg(liquidity)},
	}
	sqrt10 := sqrtAt(t, -10)
	onBoundary := V3State{
		SqrtPriceX96: sqrt10,
		Tick:         -10,
		Liquidity:    liquidity,
		TickSpacing:  10,
		Bitmap:       bitmap,
		Ticks:        ticks,
	}
	belowState := V3State{
		SqrtPriceX96: new(big.Int).Sub(sqrt10, big.NewInt(1)),
		Tick:         -11,
		Liquidity:    half,
		TickSpacing:  10,
		Bitmap:       bitmap,
		Ticks:        ticks,
	}
	amount := big.NewInt(100_000_000_000_000)

	onResult, err := SwapV3ExactIn(onBoundary, true, amount)
	if err != nil {
		t.Fatalf("on-boundary swap: %v", err)
	}
	belowResult, err := SwapV3ExactIn(belowState, true, amount)
	if err != nil {
		t.Fatalf("below-boundary swap: %v", err)
	}
	if !slices.Equal(onResult.Crossed, []int32{-10}) {
		t.Fatalf("on-boundary crossed = %v, want [-10]", onResult.Crossed)
	}
	if len(belowResult.Crossed) != 0 {
		t.Fatalf("below-boundary crossed = %v, want none", belowResult.Crossed)
	}
	outputGap := new(big.Int).Sub(onResult.AmountOut, belowResult.AmountOut)
	if new(big.Int).Abs(outputGap).Cmp(big.NewInt(5)) >= 0 {
		t.Fatalf("boundary output gap = %s, want under 5", outputGap)
	}
	if onResult.Tick != belowResult.Tick {
		t.Fatalf("final ticks differ: %d vs %d", onResult.Tick, belowResult.Tick)
	}
}

func TestSwapV3CrossingOneForZeroSeveralTicks(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	half := new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(2))
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    liquidity,
		TickSpacing:  10,
		Bitmap: map[int16]*big.Int{
			0: wordWithBits(1, 2, 4),
		},
		Ticks: map[int32]TickInfo{
			10: {LiquidityGross: new(big.Int).Mul(half, big.NewInt(3)), LiquidityNet: new(big.Int).Neg(half)},
			20: {LiquidityGross: new(big.Int).Mul(half, big.NewInt(5)), LiquidityNet: new(big.Int).Mul(half, big.NewInt(3))},
			40: {
				LiquidityGross: new(big.Int).Mul(liquidity, big.NewInt(1002)),
				LiquidityNet:   new(big.Int).Mul(liquidity, big.NewInt(998)),
			},
			200_000: {
				LiquidityGross: new(big.Int).Mul(liquidity, big.NewInt(1000)),
				LiquidityNet:   new(big.Int).Mul(liquidity, big.NewInt(1000)),
			},
		},
	}
	amount := big.NewInt(2_000_000_000_000_000)
	result, err := SwapV3ExactIn(state, false, amount)
	if err != nil {
		t.Fatalf("SwapV3ExactIn: %v", err)
	}
	if !slices.Equal(result.Crossed, []int32{10, 20}) {
		t.Fatalf("crossed = %v, want [10 20]", result.Crossed)
	}

	sqrt10 := sqrtAt(t, 10)
	sqrt20 := sqrtAt(t, 20)
	sqrt40 := sqrtAt(t, 40)
	remaining := new(big.Rat).SetInt(amount)
	total := new(big.Rat)
	segments := []struct {
		liquidity *big.Int
		sqrtLow   *big.Int
		sqrtHigh  *big.Int
	}{
		{liquidity, new(big.Int).Set(Q96), sqrt10},
		{half, sqrt10, sqrt20},
		{new(big.Int).Mul(half, big.NewInt(4)), sqrt20, sqrt40},
	}
	for _, segment := range segments {
		out, left := segmentOneForZeroOut(segment.liquidity, segment.sqrtLow, segment.sqrtHigh, remaining)
		total.Add(total, out)
		remaining = left
		if remaining.Sign() == 0 {
			break
		}
	}
	delta := new(big.Rat).Sub(new(big.Rat).SetInt(result.AmountOut), total)
	if ratAbs(delta).Cmp(new(big.Rat).SetInt64(10)) > 0 {
		t.Fatalf("amount out = %s, want within 10 of %s", result.AmountOut, total.FloatString(6))
	}
}

func segmentOneForZeroOut(liquidity, sqrtLow, sqrtHigh *big.Int, remaining *big.Rat) (*big.Rat, *big.Rat) {
	l := new(big.Rat).SetInt(liquidity)
	q := new(big.Rat).SetInt(Q96)
	low := new(big.Rat).SetInt(sqrtLow)
	high := new(big.Rat).SetInt(sqrtHigh)

	xLow := new(big.Rat).Mul(l, q)
	xLow.Quo(xLow, low)
	yLow := new(big.Rat).Mul(l, low)
	yLow.Quo(yLow, q)
	xHigh := new(big.Rat).Mul(l, q)
	xHigh.Quo(xHigh, high)
	yHigh := new(big.Rat).Mul(l, high)
	yHigh.Quo(yHigh, q)

	toBoundary := new(big.Rat).Sub(yHigh, yLow)
	if remaining.Cmp(toBoundary) <= 0 {
		out := new(big.Rat).Mul(xLow, remaining)
		out.Quo(out, new(big.Rat).Add(yLow, remaining))
		return out, new(big.Rat)
	}
	return new(big.Rat).Sub(xLow, xHigh), new(big.Rat).Sub(remaining, toBoundary)
}

func crossingState() V3State {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	half := new(big.Int).Div(new(big.Int).Set(liquidity), big.NewInt(2))
	threeHalves := new(big.Int).Mul(half, big.NewInt(3))
	fiveHalves := new(big.Int).Mul(half, big.NewInt(5))
	twice := new(big.Int).Mul(liquidity, big.NewInt(2))
	return V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    liquidity,
		TickSpacing:  10,
		Bitmap: map[int16]*big.Int{
			-1: wordWithBits(255, 254, 252),
		},
		Ticks: map[int32]TickInfo{
			-10: {
				LiquidityGross: threeHalves,
				LiquidityNet:   half,
			},
			-20: {
				LiquidityGross: fiveHalves,
				LiquidityNet:   new(big.Int).Neg(threeHalves),
			},
			-40: {
				LiquidityGross: twice,
				LiquidityNet:   twice,
			},
			10: {
				LiquidityGross: new(big.Int).Set(liquidity),
				LiquidityNet:   new(big.Int).Neg(liquidity),
			},
		},
	}
}

func segmentZeroForOneOut(liquidity, sqrtHigh, sqrtLow *big.Int, remaining *big.Rat) (*big.Rat, *big.Rat) {
	l := new(big.Rat).SetInt(liquidity)
	q := new(big.Rat).SetInt(Q96)
	high := new(big.Rat).SetInt(sqrtHigh)
	low := new(big.Rat).SetInt(sqrtLow)

	xHigh := new(big.Rat).Mul(l, q)
	xHigh.Quo(xHigh, high)
	yHigh := new(big.Rat).Mul(l, high)
	yHigh.Quo(yHigh, q)
	xLow := new(big.Rat).Mul(l, q)
	xLow.Quo(xLow, low)
	yLow := new(big.Rat).Mul(l, low)
	yLow.Quo(yLow, q)

	toBoundary := new(big.Rat).Sub(xLow, xHigh)
	if remaining.Cmp(toBoundary) <= 0 {
		out := new(big.Rat).Mul(yHigh, remaining)
		out.Quo(out, new(big.Rat).Add(xHigh, remaining))
		return out, new(big.Rat)
	}
	return new(big.Rat).Sub(yHigh, yLow), new(big.Rat).Sub(remaining, toBoundary)
}

// A zeroForOne swap that ends exactly on an initialized tick leaves V3 at
// tick = boundary − 1 with the price on the boundary (UniswapV3Pool sets
// tick = tickNext − 1). A real USDG/WETH pool can rest there (tick −198409,
// sqrt price at −198408), and quoting must not reject it as an invalid state.
// It is the same pool as the one priced from above the boundary, so both must
// quote alike in either direction.
func TestSwapV3QuotesFromALowerTickBoundary(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	ticks := func() map[int32]TickInfo {
		return map[int32]TickInfo{10: {LiquidityGross: new(big.Int).Set(liquidity), LiquidityNet: new(big.Int).Div(liquidity, big.NewInt(2))}}
	}
	above := new(big.Int).Add(liquidity, new(big.Int).Div(liquidity, big.NewInt(2)))
	boundary := V3State{SqrtPriceX96: sqrtAt(t, 10), Tick: 9, Liquidity: new(big.Int).Set(liquidity), TickSpacing: 10, Bitmap: map[int16]*big.Int{0: wordWithBits(1)}, Ticks: ticks()}
	upper := V3State{SqrtPriceX96: sqrtAt(t, 10), Tick: 10, Liquidity: above, TickSpacing: 10, Bitmap: map[int16]*big.Int{0: wordWithBits(1)}, Ticks: ticks()}
	for _, zeroForOne := range []bool{true, false} {
		amount := big.NewInt(1_000_000_000_000)
		got, err := SwapV3ExactIn(boundary, zeroForOne, amount)
		if err != nil {
			t.Fatalf("zeroForOne %v from the boundary: %v", zeroForOne, err)
		}
		want, err := SwapV3ExactIn(upper, zeroForOne, amount)
		if err != nil {
			t.Fatalf("zeroForOne %v from above: %v", zeroForOne, err)
		}
		if got.AmountOut.Cmp(want.AmountOut) != 0 {
			t.Fatalf("zeroForOne %v: %s out from the boundary, %s from above", zeroForOne, got.AmountOut, want.AmountOut)
		}
	}
}
