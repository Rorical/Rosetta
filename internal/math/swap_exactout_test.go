package math

import (
	"errors"
	"math/big"
	"slices"
	"testing"
)

func TestSwapV3ExactOutRealVector(t *testing.T) {
	state := V3State{
		SqrtPriceX96: bigFromString(t, "31861902116756893205048"),
		Tick:         -294544,
		Liquidity:    bigFromString(t, "707696824048886038"),
		FeePips:      500,
		TickSpacing:  10,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}
	requested := bigFromString(t, "26419448326905695269")
	preSqrt := new(big.Int).Set(state.SqrtPriceX96)
	preLiquidity := new(big.Int).Set(state.Liquidity)

	result, err := SwapV3ExactOut(state, false, requested)
	if err != nil {
		t.Fatalf("SwapV3ExactOut: %v", err)
	}
	if result.AmountInUsed.Cmp(big.NewInt(4274957)) != 0 {
		t.Fatalf("amount in used = %s, want 4274957", result.AmountInUsed)
	}
	if result.AmountOut.Cmp(requested) != 0 {
		t.Fatalf("amount out = %s, want %s", result.AmountOut, requested)
	}
	if result.SqrtPriceX96.String() != "31862380467918237550733" {
		t.Fatalf("sqrt price = %s, want 31862380467918237550733", result.SqrtPriceX96)
	}
	if result.Tick != -294544 {
		t.Fatalf("tick = %d, want -294544", result.Tick)
	}
	if result.Liquidity.String() != "707696824048886038" {
		t.Fatalf("liquidity = %s, want 707696824048886038", result.Liquidity)
	}
	if len(result.Crossed) != 0 {
		t.Fatalf("crossed = %v, want none", result.Crossed)
	}
	if result.HitLimit {
		t.Fatal("unexpected price limit")
	}
	if state.SqrtPriceX96.Cmp(preSqrt) != 0 || state.Liquidity.Cmp(preLiquidity) != 0 {
		t.Fatal("SwapV3ExactOut mutated the pre-state")
	}
}

func TestSwapV3ExactOutCrossing(t *testing.T) {
	state := crossingState()
	exactIn, err := SwapV3ExactIn(state, true, big.NewInt(2_000_000_000_000_000))
	if err != nil {
		t.Fatalf("SwapV3ExactIn: %v", err)
	}

	result, err := SwapV3ExactOut(state, true, exactIn.AmountOut)
	if err != nil {
		t.Fatalf("SwapV3ExactOut: %v", err)
	}
	if result.AmountOut.Cmp(exactIn.AmountOut) != 0 {
		t.Fatalf("amount out = %s, want %s", result.AmountOut, exactIn.AmountOut)
	}
	if result.AmountInUsed.Cmp(exactIn.AmountInUsed) != 0 {
		t.Fatalf("amount in used = %s, want %s", result.AmountInUsed, exactIn.AmountInUsed)
	}
	if result.SqrtPriceX96.String() != "79099600937193653947886179901" {
		t.Fatalf("sqrt price = %s, want 79099600937193653947886179901", result.SqrtPriceX96)
	}
	if result.Tick != -33 {
		t.Fatalf("tick = %d, want -33", result.Tick)
	}
	if result.Liquidity.Cmp(big.NewInt(2_000_000_000_000_000_000)) != 0 {
		t.Fatalf("liquidity = %s, want 2000000000000000000", result.Liquidity)
	}
	if !slices.Equal(result.Crossed, []int32{-10, -20}) {
		t.Fatalf("crossed = %v, want [-10 -20]", result.Crossed)
	}
	if result.HitLimit {
		t.Fatal("unexpected price limit")
	}
	// Exact-output rounds the price up, so it ends above the exact-input replay.
	if result.SqrtPriceX96.Cmp(exactIn.SqrtPriceX96) <= 0 {
		t.Fatalf("exact-out sqrt %s is not above exact-in sqrt %s", result.SqrtPriceX96, exactIn.SqrtPriceX96)
	}
}

func TestSwapV3ExactOutWithFee(t *testing.T) {
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000_000_000_000_000),
		FeePips:      3000,
		TickSpacing:  60,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}
	tests := []struct {
		name       string
		zeroForOne bool
		wantSqrt   string
		wantTick   int32
	}{
		{name: "token0 out", zeroForOne: false, wantSqrt: "79228241742506080099624049961", wantTick: 0},
		{name: "token1 out", zeroForOne: true, wantSqrt: "79228083286101823329206356792", wantTick: -1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := SwapV3ExactOut(state, test.zeroForOne, big.NewInt(1_000_000_000_000))
			if err != nil {
				t.Fatalf("SwapV3ExactOut: %v", err)
			}
			if result.AmountInUsed.Cmp(big.NewInt(1_003_010_030_093)) != 0 {
				t.Fatalf("amount in used = %s, want 1003010030093", result.AmountInUsed)
			}
			if result.AmountOut.Cmp(big.NewInt(1_000_000_000_000)) != 0 {
				t.Fatalf("amount out = %s, want 1000000000000", result.AmountOut)
			}
			if result.SqrtPriceX96.String() != test.wantSqrt {
				t.Fatalf("sqrt price = %s, want %s", result.SqrtPriceX96, test.wantSqrt)
			}
			if result.Tick != test.wantTick {
				t.Fatalf("tick = %d, want %d", result.Tick, test.wantTick)
			}
			if result.Liquidity.Cmp(state.Liquidity) != 0 {
				t.Fatalf("liquidity = %s, want %s", result.Liquidity, state.Liquidity)
			}
			if result.HitLimit {
				t.Fatal("unexpected price limit")
			}
		})
	}
}

func TestSwapV3ExactOutPartialStep(t *testing.T) {
	liquidity := big.NewInt(1_000_000_000_000_000_000)
	amountOut := big.NewInt(1_000_000_000_000)
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    liquidity,
		TickSpacing:  60,
		Bitmap: map[int16]*big.Int{
			0:  wordWithBits(1),
			-1: wordWithBits(255),
		},
		Ticks: map[int32]TickInfo{
			60:  {LiquidityGross: new(big.Int).Set(liquidity), LiquidityNet: new(big.Int).Neg(liquidity)},
			-60: {LiquidityGross: new(big.Int).Set(liquidity), LiquidityNet: new(big.Int).Set(liquidity)},
		},
	}

	next0, err := NextSqrtPriceFromAmount0Out(Q96, liquidity, amountOut)
	if err != nil {
		t.Fatalf("NextSqrtPriceFromAmount0Out: %v", err)
	}
	next1, err := NextSqrtPriceFromAmount1Out(Q96, liquidity, amountOut)
	if err != nil {
		t.Fatalf("NextSqrtPriceFromAmount1Out: %v", err)
	}
	in0, err := amount1Delta(Q96, next0, liquidity, true)
	if err != nil {
		t.Fatalf("amount1Delta: %v", err)
	}
	in1, err := amount0Delta(next1, Q96, liquidity, true)
	if err != nil {
		t.Fatalf("amount0Delta: %v", err)
	}

	tests := []struct {
		name       string
		zeroForOne bool
		wantSqrt   *big.Int
		wantIn     *big.Int
	}{
		{name: "token0 out", zeroForOne: false, wantSqrt: next0, wantIn: in0},
		{name: "token1 out", zeroForOne: true, wantSqrt: next1, wantIn: in1},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result, err := SwapV3ExactOut(state, test.zeroForOne, amountOut)
			if err != nil {
				t.Fatalf("SwapV3ExactOut: %v", err)
			}
			if result.AmountOut.Cmp(amountOut) != 0 {
				t.Fatalf("amount out = %s, want the full request %s", result.AmountOut, amountOut)
			}
			if result.SqrtPriceX96.Cmp(test.wantSqrt) != 0 {
				t.Fatalf("sqrt price = %s, want %s", result.SqrtPriceX96, test.wantSqrt)
			}
			if result.AmountInUsed.Cmp(test.wantIn) != 0 {
				t.Fatalf("amount in used = %s, want %s", result.AmountInUsed, test.wantIn)
			}
			if len(result.Crossed) != 0 {
				t.Fatalf("crossed = %v, want none", result.Crossed)
			}
			if result.HitLimit {
				t.Fatal("unexpected price limit")
			}
		})
	}
}

func TestSwapV3ExactOutErrors(t *testing.T) {
	base := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000_000_000_000_000),
		TickSpacing:  60,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}
	noLiquidity := base
	noLiquidity.Liquidity = new(big.Int)
	windowed := base
	windowed.WindowMin = -1
	windowed.WindowMax = 0

	crossing := crossingState()
	exactIn, err := SwapV3ExactIn(crossing, true, big.NewInt(2_000_000_000_000_000))
	if err != nil {
		t.Fatalf("SwapV3ExactIn: %v", err)
	}
	delete(crossing.Ticks, -10)

	tests := []struct {
		name      string
		state     V3State
		amountOut *big.Int
		wantErr   error
	}{
		{name: "zero output", state: base, amountOut: new(big.Int), wantErr: ErrInvalidArgument},
		{name: "negative output", state: base, amountOut: big.NewInt(-1), wantErr: ErrInvalidArgument},
		{name: "zero liquidity", state: noLiquidity, amountOut: big.NewInt(1_000_000), wantErr: ErrInvalidArgument},
		{
			name:      "out of window",
			state:     windowed,
			amountOut: big.NewInt(1_000_000_000_000_000),
			wantErr:   ErrOutOfWindow,
		},
		{
			name:      "missing tick",
			state:     crossing,
			amountOut: exactIn.AmountOut,
			wantErr:   ErrMissingTick,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SwapV3ExactOut(test.state, true, test.amountOut); !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestSwapV3ExactOutHitsLimit(t *testing.T) {
	state := V3State{
		SqrtPriceX96: new(big.Int).Set(Q96),
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000_000_000_000_000),
		TickSpacing:  60,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}
	requested := big.NewInt(2_000_000_000_000_000_000)
	result, err := SwapV3ExactOut(state, false, requested)
	if err != nil {
		t.Fatalf("SwapV3ExactOut: %v", err)
	}
	if !result.HitLimit {
		t.Fatal("HitLimit = false, want true")
	}
	if result.AmountOut.Cmp(requested) != 0 {
		t.Fatalf("amount out = %s, want requested %s", result.AmountOut, requested)
	}
	if result.AmountInUsed.Sign() <= 0 {
		t.Fatalf("amount in used = %s, want positive", result.AmountInUsed)
	}
}
