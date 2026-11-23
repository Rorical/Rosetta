package math

import (
	"errors"
	"math/big"
	"slices"
	"testing"
)

// limitSwapState is the ARB/USDC 0.05% pre-block state from the real-vector
// tests. Its loaded bitmap word holds no initialized ticks, so a swap moves
// freely until it reaches a price limit.
func limitSwapState(t *testing.T) V3State {
	t.Helper()
	return V3State{
		SqrtPriceX96: bigFromString(t, "31869754766453301575680"),
		Tick:         -294539,
		Liquidity:    bigFromString(t, "707696824048886038"),
		FeePips:      500,
		TickSpacing:  10,
		Bitmap:       map[int16]*big.Int{},
		Ticks:        map[int32]TickInfo{},
	}
}

func TestSwapV3ExactInWithLimitNotHit(t *testing.T) {
	state := limitSwapState(t)
	// The pool's liquidity is small: 1e6 wei of either token moves the price by
	// a fraction of a tick, far short of either limit below.
	amountIn := bigFromString(t, "1000000")

	tests := []struct {
		name       string
		zeroForOne bool
		limit      *big.Int
	}{
		{name: "zero for one", zeroForOne: true, limit: sqrtAt(t, -300000)},
		{name: "one for zero", zeroForOne: false, limit: sqrtAt(t, -290000)},
		{name: "nil limit", zeroForOne: true, limit: nil},
		{name: "zero limit", zeroForOne: true, limit: new(big.Int)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unlimited, err := SwapV3ExactIn(state, test.zeroForOne, amountIn)
			if err != nil {
				t.Fatalf("SwapV3ExactIn: %v", err)
			}
			limited, err := SwapV3ExactInWithLimit(state, test.zeroForOne, amountIn, test.limit)
			if err != nil {
				t.Fatalf("SwapV3ExactInWithLimit: %v", err)
			}
			assertSameSwap(t, limited, unlimited)
			if limited.HitLimit {
				t.Fatal("limit reported as hit below the limit")
			}
		})
	}
}

func TestSwapV3ExactOutWithLimitNotHit(t *testing.T) {
	state := limitSwapState(t)
	amountOut := bigFromString(t, "1000000")

	tests := []struct {
		name       string
		zeroForOne bool
		limit      *big.Int
	}{
		{name: "zero for one", zeroForOne: true, limit: sqrtAt(t, -300000)},
		{name: "one for zero", zeroForOne: false, limit: sqrtAt(t, -290000)},
		{name: "nil limit", zeroForOne: false, limit: nil},
		{name: "zero limit", zeroForOne: false, limit: new(big.Int)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unlimited, err := SwapV3ExactOut(state, test.zeroForOne, amountOut)
			if err != nil {
				t.Fatalf("SwapV3ExactOut: %v", err)
			}
			limited, err := SwapV3ExactOutWithLimit(state, test.zeroForOne, amountOut, test.limit)
			if err != nil {
				t.Fatalf("SwapV3ExactOutWithLimit: %v", err)
			}
			assertSameSwap(t, limited, unlimited)
			if limited.HitLimit {
				t.Fatal("limit reported as hit below the limit")
			}
		})
	}
}

func TestSwapV3ExactInWithLimitHit(t *testing.T) {
	state := limitSwapState(t)
	amountIn := bigPow10(40)

	tests := []struct {
		name       string
		zeroForOne bool
		limitTick  int32
	}{
		{name: "zero for one", zeroForOne: true, limitTick: -294600},
		{name: "one for zero", zeroForOne: false, limitTick: -294000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit := sqrtAt(t, test.limitTick)
			result, err := SwapV3ExactInWithLimit(state, test.zeroForOne, amountIn, limit)
			if err != nil {
				t.Fatalf("SwapV3ExactInWithLimit: %v", err)
			}
			if !result.HitLimit {
				t.Fatal("limit not reported as hit")
			}
			if result.SqrtPriceX96.Cmp(limit) != 0 {
				t.Fatalf("sqrt price = %s, want the limit %s", result.SqrtPriceX96, limit)
			}
			if result.Tick != test.limitTick {
				t.Fatalf("tick = %d, want %d", result.Tick, test.limitTick)
			}
			if result.AmountInUsed.Cmp(amountIn) >= 0 {
				t.Fatalf("used input %s did not leave input remaining", result.AmountInUsed)
			}
			if result.AmountOut.Sign() <= 0 {
				t.Fatalf("amount out = %s, want positive", result.AmountOut)
			}
		})
	}
}

func TestSwapV3ExactOutWithLimitHit(t *testing.T) {
	state := limitSwapState(t)
	requested := bigPow10(40)

	tests := []struct {
		name       string
		zeroForOne bool
		limitTick  int32
	}{
		{name: "zero for one", zeroForOne: true, limitTick: -294600},
		{name: "one for zero", zeroForOne: false, limitTick: -294000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			limit := sqrtAt(t, test.limitTick)
			result, err := SwapV3ExactOutWithLimit(state, test.zeroForOne, requested, limit)
			if err != nil {
				t.Fatalf("SwapV3ExactOutWithLimit: %v", err)
			}
			if !result.HitLimit {
				t.Fatal("limit not reported as hit")
			}
			if result.SqrtPriceX96.Cmp(limit) != 0 {
				t.Fatalf("sqrt price = %s, want the limit %s", result.SqrtPriceX96, limit)
			}
			if result.Tick != test.limitTick {
				t.Fatalf("tick = %d, want %d", result.Tick, test.limitTick)
			}
			if result.AmountOut.Cmp(requested) != 0 {
				t.Fatalf("amount out = %s, want the request %s", result.AmountOut, requested)
			}
			if result.AmountInUsed.Sign() <= 0 {
				t.Fatalf("amount in used = %s, want positive", result.AmountInUsed)
			}
		})
	}
}

func TestSwapV3WithLimitRejectsInvalidSide(t *testing.T) {
	state := limitSwapState(t)
	current := state.SqrtPriceX96
	amount := big.NewInt(1_000_000)

	tests := []struct {
		name       string
		zeroForOne bool
		limit      *big.Int
	}{
		{name: "negative", zeroForOne: true, limit: big.NewInt(-1)},
		{name: "zero for one at current price", zeroForOne: true, limit: new(big.Int).Set(current)},
		{name: "zero for one above current price", zeroForOne: true, limit: new(big.Int).Add(current, one)},
		{name: "zero for one at minimum", zeroForOne: true, limit: new(big.Int).Set(minSqrtRatioX96)},
		{name: "zero for one below minimum", zeroForOne: true, limit: new(big.Int).Sub(minSqrtRatioX96, one)},
		{name: "one for zero at current price", zeroForOne: false, limit: new(big.Int).Set(current)},
		{name: "one for zero below current price", zeroForOne: false, limit: new(big.Int).Sub(current, one)},
		{name: "one for zero at maximum", zeroForOne: false, limit: new(big.Int).Set(maxSqrtRatioX96)},
		{name: "one for zero above maximum", zeroForOne: false, limit: new(big.Int).Add(maxSqrtRatioX96, one)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := SwapV3ExactInWithLimit(state, test.zeroForOne, amount, test.limit)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("exact in error = %v, want ErrInvalidArgument", err)
			}
			_, err = SwapV3ExactOutWithLimit(state, test.zeroForOne, amount, test.limit)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("exact out error = %v, want ErrInvalidArgument", err)
			}
		})
	}
}

func bigPow10(exponent int64) *big.Int {
	return new(big.Int).Exp(big.NewInt(10), big.NewInt(exponent), nil)
}

func assertSameSwap(t *testing.T, got, want SwapResult) {
	t.Helper()
	if got.AmountOut.Cmp(want.AmountOut) != 0 {
		t.Fatalf("amount out = %s, want %s", got.AmountOut, want.AmountOut)
	}
	if got.AmountInUsed.Cmp(want.AmountInUsed) != 0 {
		t.Fatalf("amount in used = %s, want %s", got.AmountInUsed, want.AmountInUsed)
	}
	if got.SqrtPriceX96.Cmp(want.SqrtPriceX96) != 0 {
		t.Fatalf("sqrt price = %s, want %s", got.SqrtPriceX96, want.SqrtPriceX96)
	}
	if got.Liquidity.Cmp(want.Liquidity) != 0 {
		t.Fatalf("liquidity = %s, want %s", got.Liquidity, want.Liquidity)
	}
	if got.Tick != want.Tick {
		t.Fatalf("tick = %d, want %d", got.Tick, want.Tick)
	}
	if !slices.Equal(got.Crossed, want.Crossed) {
		t.Fatalf("crossed = %v, want %v", got.Crossed, want.Crossed)
	}
	if got.HitLimit != want.HitLimit {
		t.Fatalf("hit limit = %t, want %t", got.HitLimit, want.HitLimit)
	}
}
