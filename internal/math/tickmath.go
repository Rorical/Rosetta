package math

import (
	"fmt"
	"math/big"

	"github.com/holiman/uint256"
)

// tickRatioHex holds 1.0001^(2^i) in Q128.128 for i in [0, 19], truncated to
// 128 fractional bits. These are the Uniswap V3 TickMath constants.
var tickRatioHex = [20]string{
	"fffcb933bd6fad37aa2d162d1a594001",
	"fff97272373d413259a46990580e213a",
	"fff2e50f5f656932ef12357cf3c7fdcc",
	"ffe5caca7e10e4e61c3624eaa0941cd0",
	"ffcb9843d60f6159c9db58835c926644",
	"ff973b41fa98c081472e6896dfb254c0",
	"ff2ea16466c96a3843ec78b326b52861",
	"fe5dee046a99a2a811c461f1969c3053",
	"fcbe86c7900a88aedcffc83b479aa3a4",
	"f987a7253ac413176f2b074cf7815e54",
	"f3392b0822b70005940c7a398e4b70f3",
	"e7159475a2c29b7443b29c7fa6e889d9",
	"d097f3bdfd2022b8845ad8f792aa5825",
	"a9f746462d870fdf8a65dc1f90e061e5",
	"70d869a156d2a1b890bb3df62baf32f7",
	"31be135f97d08fd981231505542fcfa6",
	"09aa508b5b7a84e1c677de54f3e99bc9",
	"005d6af8dedb81196699c329225ee604",
	"00002216e584f5fa1ea926041bedfe98",
	"00000048a170391f7dc42444e8fa2",
}

// The fixed-width tables are operands only and must never be mutated.
var (
	minSqrtRatioX96 *big.Int
	maxSqrtRatioX96 *big.Int

	tickRatios256      [20]uint256.Int
	minSqrtRatio256    uint256.Int
	maxSqrtRatio256    uint256.Int
	logSqrt10001Mul256 uint256.Int
	tickLowOffset256   uint256.Int
	tickHighOffset256  uint256.Int

	tickMathTablesOK bool
)

func init() {
	ok := true
	load := func(dst *uint256.Int, s string, base int) *big.Int {
		v, parsed := new(big.Int).SetString(s, base)
		if !parsed || v.BitLen() > 256 {
			ok = false
			return nil
		}
		dst.SetFromBig(v)
		return v
	}

	minSqrtRatioX96 = load(&minSqrtRatio256, "4295128739", 10)
	maxSqrtRatioX96 = load(&maxSqrtRatio256, "1461446703485210103287273052203988822378723970342", 10)
	load(&logSqrt10001Mul256, "255738958999603826347141", 10)
	load(&tickLowOffset256, "3402992956809132418596140100660247210", 10)
	load(&tickHighOffset256, "291339464771989622907027621153398088495", 10)
	for i := range tickRatioHex {
		load(&tickRatios256[i], tickRatioHex[i], 16)
	}
	tickMathTablesOK = ok
}

// calculateTickPrice matches Uniswap V3 TickMath.getSqrtRatioAtTick.
func calculateTickPrice(tick int32) (*big.Int, error) {
	if tick < MinTick || tick > MaxTick {
		return nil, fmt.Errorf("%w: %d", ErrTickOutOfRange, tick)
	}
	if !tickMathTablesOK {
		return nil, fmt.Errorf("%w: tick math constants failed to initialize", ErrInvalidState)
	}
	var sqrtPrice uint256.Int
	sqrtRatioAtTick(&sqrtPrice, tick)
	return sqrtPrice.ToBig(), nil
}

// sqrtRatioAtTick sets z to TickMath.getSqrtRatioAtTick(tick) for a tick in
// [MinTick, MaxTick], so z lands in [MIN_SQRT_RATIO, MAX_SQRT_RATIO].
func sqrtRatioAtTick(z *uint256.Int, tick int32) {
	absTick := uint32(tick)
	if tick < 0 {
		absTick = uint32(-tick)
	}

	// The Solidity reference starts from 2^128 and replaces it by
	// (ratio*constant)>>128 for each set bit. Every constant is below 2^128,
	// so the first multiplication just selects its constant and later ones
	// keep ratio below 2^128, where only the high half of a 128x128-bit
	// product is needed.
	var ratio uint256.Int
	ratio[2] = 1
	started := false
	for bit := range len(tickRatios256) {
		if absTick&(1<<bit) == 0 {
			continue
		}
		constant := &tickRatios256[bit]
		if !started {
			ratio = *constant
			started = true
			continue
		}
		ratio[1], ratio[0], _, _ = mul128(ratio[1], ratio[0], constant[1], constant[0])
	}
	if tick > 0 {
		ratio.Div(u256Max, &ratio)
	}

	roundUp := uint32(ratio[0]) != 0
	z.Rsh(&ratio, 32)
	if roundUp {
		z.AddUint64(z, 1)
	}
}

// SqrtPriceX96ToTick returns the greatest tick whose sqrt price is at or below
// sqrtPriceX96, matching Uniswap V3 TickMath.getTickAtSqrtRatio on the
// protocol's price range. The exact upper bound is accepted as MaxTick so tick
// round trips are total; a pool never stores that price.
func SqrtPriceX96ToTick(sqrtPriceX96 *big.Int) (int32, error) {
	if err := checkPositive("sqrt price", sqrtPriceX96); err != nil {
		return 0, err
	}
	if !tickMathTablesOK {
		return 0, fmt.Errorf("%w: tick math constants failed to initialize", ErrInvalidState)
	}
	if sqrtPriceX96.Cmp(maxSqrtRatioX96) == 0 {
		return MaxTick, nil
	}
	if sqrtPriceX96.Cmp(minSqrtRatioX96) < 0 || sqrtPriceX96.Cmp(maxSqrtRatioX96) > 0 {
		return 0, fmt.Errorf("%w: %s", ErrSqrtPriceOutOfRange, sqrtPriceX96)
	}
	var sqrtPrice uint256.Int
	sqrtPrice.SetFromBig(sqrtPriceX96)
	return tickAtSqrtRatio(&sqrtPrice), nil
}

// tickAtSqrtRatio is TickMath.getTickAtSqrtRatio for a price in
// [MIN_SQRT_RATIO, MAX_SQRT_RATIO). Signed intermediates are two's
// complement, as in the Solidity reference.
func tickAtSqrtRatio(sqrtPrice *uint256.Int) int32 {
	var ratio, r uint256.Int
	ratio.Lsh(sqrtPrice, 32)
	msb := ratio.BitLen() - 1
	if msb >= 128 {
		r.Rsh(&ratio, uint(msb-127))
	} else {
		r.Lsh(&ratio, uint(127-msb))
	}

	// log2 is signed Q64.64: integer part msb-128, then 14 fractional bits
	// from repeatedly squaring the normalized mantissa r, which has bit 127
	// set and stays below 2^128. The reference sets r = r*r >> 127 and, when
	// that reaches bit 128, records the bit and halves r to r*r >> 128.
	var fraction uint64
	mantissaHigh, mantissaLow := r[1], r[0]
	for shift := 63; shift >= 50; shift-- {
		word3, word2, word1, _ := mul128(mantissaHigh, mantissaLow, mantissaHigh, mantissaLow)
		if word3>>63 != 0 {
			fraction |= 1 << shift
			mantissaHigh, mantissaLow = word3, word2
		} else {
			mantissaHigh, mantissaLow = word3<<1|word2>>63, word2<<1|word1>>63
		}
	}
	var logSqrt10001 uint256.Int
	logSqrt10001[0] = fraction
	logSqrt10001[1] = uint64(int64(msb - 128))
	if msb < 128 {
		logSqrt10001[2], logSqrt10001[3] = ^uint64(0), ^uint64(0)
	}
	logSqrt10001.Mul(&logSqrt10001, &logSqrt10001Mul256)

	var low, high uint256.Int
	low.Sub(&logSqrt10001, &tickLowOffset256)
	low.SRsh(&low, 128)
	high.Add(&logSqrt10001, &tickHighOffset256)
	high.SRsh(&high, 128)
	tickLow, tickHigh := int32(int64(low[0])), int32(int64(high[0]))
	if tickLow == tickHigh {
		return tickLow
	}
	var sqrtAtHigh uint256.Int
	sqrtRatioAtTick(&sqrtAtHigh, tickHigh)
	if !sqrtAtHigh.Gt(sqrtPrice) {
		return tickHigh
	}
	return tickLow
}
