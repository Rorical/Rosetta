package math

import (
	"fmt"
	"math/big"
	"math/rand"
	"testing"
)

// The arbitrary-precision TickMath this package used before the fixed-width
// rewrite. It is the differential reference: any disagreement is a bug in the
// fixed-width code.
var (
	referenceTickRatios     [20]*big.Int
	referenceLogMultiplier  = mustBig("255738958999603826347141", 10)
	referenceTickLowOffset  = mustBig("3402992956809132418596140100660247210", 10)
	referenceTickHighOffset = mustBig("291339464771989622907027621153398088495", 10)
)

func init() {
	for i := range tickRatioHex {
		referenceTickRatios[i] = mustBig(tickRatioHex[i], 16)
	}
}

func mustBig(s string, base int) *big.Int {
	v, ok := new(big.Int).SetString(s, base)
	if !ok {
		panic(fmt.Sprintf("invalid test constant %q", s))
	}
	return v
}

func referenceTickPrice(tick int32) (*big.Int, error) {
	if tick < MinTick || tick > MaxTick {
		return nil, fmt.Errorf("%w: %d", ErrTickOutOfRange, tick)
	}
	absTick := tick
	if absTick < 0 {
		absTick = -absTick
	}
	ratio := new(big.Int).Lsh(one, 128)
	if absTick&1 != 0 {
		ratio.Set(referenceTickRatios[0])
	}
	for bit := 1; bit < len(referenceTickRatios); bit++ {
		if absTick&(1<<bit) != 0 {
			ratio.Mul(ratio, referenceTickRatios[bit])
			ratio.Rsh(ratio, 128)
		}
	}
	if tick > 0 {
		ratio.Quo(uint256Max, ratio)
	}
	sqrtPrice := new(big.Int).Rsh(ratio, 32)
	if ratio.TrailingZeroBits() < 32 {
		sqrtPrice.Add(sqrtPrice, one)
	}
	if sqrtPrice.Cmp(minSqrtRatioX96) < 0 || sqrtPrice.Cmp(maxSqrtRatioX96) > 0 {
		return nil, fmt.Errorf("%w: tick %d produced %s", ErrSqrtPriceOutOfRange, tick, sqrtPrice)
	}
	return sqrtPrice, nil
}

func referenceSqrtPriceX96ToTick(sqrtPriceX96 *big.Int) (int32, error) {
	if err := checkPositive("sqrt price", sqrtPriceX96); err != nil {
		return 0, err
	}
	if sqrtPriceX96.Cmp(maxSqrtRatioX96) == 0 {
		return MaxTick, nil
	}
	if sqrtPriceX96.Cmp(minSqrtRatioX96) < 0 || sqrtPriceX96.Cmp(maxSqrtRatioX96) > 0 {
		return 0, fmt.Errorf("%w: %s", ErrSqrtPriceOutOfRange, sqrtPriceX96)
	}
	ratio := new(big.Int).Lsh(sqrtPriceX96, 32)
	msb := ratio.BitLen() - 1
	r := new(big.Int)
	if msb >= 128 {
		r.Rsh(ratio, uint(msb-127))
	} else {
		r.Lsh(ratio, uint(127-msb))
	}
	log2 := big.NewInt(int64(msb - 128))
	log2.Lsh(log2, 64)
	for shift := 63; shift >= 50; shift-- {
		r.Mul(r, r)
		r.Rsh(r, 127)
		if r.Bit(128) != 0 {
			log2.SetBit(log2, shift, 1)
			r.Rsh(r, 1)
		}
	}
	logSqrt10001 := new(big.Int).Mul(log2, referenceLogMultiplier)
	tickLow := new(big.Int).Sub(logSqrt10001, referenceTickLowOffset)
	tickLow.Rsh(tickLow, 128)
	tickHigh := new(big.Int).Add(logSqrt10001, referenceTickHighOffset)
	tickHigh.Rsh(tickHigh, 128)
	if tickLow.Cmp(tickHigh) == 0 {
		return referenceTickFromBig(tickLow)
	}
	high, err := referenceTickFromBig(tickHigh)
	if err != nil {
		return 0, err
	}
	sqrtAtHigh, err := referenceTickPrice(high)
	if err != nil {
		return 0, err
	}
	if sqrtAtHigh.Cmp(sqrtPriceX96) <= 0 {
		return high, nil
	}
	return referenceTickFromBig(tickLow)
}

func referenceTickFromBig(v *big.Int) (int32, error) {
	if !v.IsInt64() {
		return 0, fmt.Errorf("%w: tick %s does not fit int64", ErrSqrtPriceOutOfRange, v)
	}
	tick := v.Int64()
	if tick < int64(MinTick) || tick > int64(MaxTick) {
		return 0, fmt.Errorf("%w: %d", ErrTickOutOfRange, tick)
	}
	return int32(tick), nil
}

// Every tick near the range bounds and zero, and every 61st tick elsewhere: the
// price, and the inverse at the price, one wei below it and one wei above it.
// A sweep of every tick in the range also matched when this was written; it
// takes about a minute.
func TestFixedTickMathMatchesReferenceAcrossTicks(t *testing.T) {
	const dense = 4096
	cases := 0
	for tick := MinTick; tick <= MaxTick; tick++ {
		nearBound := tick < MinTick+dense || tick > MaxTick-dense || (tick > -dense && tick < dense)
		if !nearBound && tick%61 != 0 {
			continue
		}
		want, err := referenceTickPrice(tick)
		if err != nil {
			t.Fatalf("reference tick %d: %v", tick, err)
		}
		got, err := calculateTickPrice(tick)
		if err != nil || got.Cmp(want) != 0 {
			t.Fatalf("tick %d price = %v, %v; want %s", tick, got, err, want)
		}
		for _, delta := range []int64{-1, 0, 1} {
			price := new(big.Int).Add(want, big.NewInt(delta))
			compareTickAt(t, price)
			cases++
		}
	}
	t.Logf("%d inverse cases", cases)
}

func TestFixedTickAtPriceMatchesReferenceOnRandomPrices(t *testing.T) {
	random := rand.New(rand.NewSource(20261102))
	span := new(big.Int).Sub(maxSqrtRatioX96, minSqrtRatioX96)
	for range 50_000 {
		var price *big.Int
		switch random.Intn(3) {
		case 0: // Uniform over the price range: concentrates near the top.
			price = new(big.Int).Rand(random, span)
			price.Add(price, minSqrtRatioX96)
		case 1: // Uniform over bit lengths.
			bits := 32 + random.Intn(129)
			price = new(big.Int).Rand(random, new(big.Int).Lsh(one, uint(bits)))
		default: // Near a random tick boundary.
			tick := MinTick + int32(random.Intn(int(MaxTick-MinTick)+1))
			price, _ = referenceTickPrice(tick)
			price = new(big.Int).Add(price, big.NewInt(int64(random.Intn(2001)-1000)))
		}
		compareTickAt(t, price)
	}
	for _, price := range []*big.Int{
		big.NewInt(0), big.NewInt(1),
		new(big.Int).Sub(minSqrtRatioX96, one), minSqrtRatioX96, new(big.Int).Add(minSqrtRatioX96, one),
		new(big.Int).Sub(maxSqrtRatioX96, one), maxSqrtRatioX96, new(big.Int).Add(maxSqrtRatioX96, one),
		new(big.Int).Set(uint256Max),
	} {
		compareTickAt(t, price)
	}
}

func compareTickAt(t *testing.T, price *big.Int) {
	t.Helper()
	want, wantErr := referenceSqrtPriceX96ToTick(price)
	got, err := SqrtPriceX96ToTick(price)
	if fmt.Sprint(err) != fmt.Sprint(wantErr) || got != want {
		t.Fatalf("price %s tick = %d, %v; want %d, %v", price, got, err, want, wantErr)
	}
}
