package math

import (
	"math/big"
	"math/bits"

	"github.com/holiman/uint256"
)

// Fixed-width copies of the shared constants. Like their big.Int
// counterparts they are operands only and must never be mutated. The tick
// math tables and sqrt ratio bounds are loaded with the big.Int ones.
var (
	u256One        = uint256.NewInt(1)
	u256Q96        = new(uint256.Int).Lsh(u256One, Q96Bits)
	u256Max        = new(uint256.Int).SetAllOne()
	u256Uint160Max = new(uint256.Int).Sub(new(uint256.Int).Lsh(u256One, 160), u256One)
	u256FeeDivisor = uint256.NewInt(feePipsDenominator)
)

// mulDiv256 sets z to x*y/d rounded down, or up when roundUp is set, through a
// 512-bit product like FullMath.mulDiv and mulDivRoundingUp. It reports false,
// leaving z unspecified, when d is zero or the quotient needs more than 256
// bits. z may alias any operand.
func mulDiv256(z, x, y, d *uint256.Int, roundUp bool) bool {
	if d.IsZero() {
		return false
	}
	divisor := *d
	var low uint256.Int
	if _, overflow := low.MulOverflow(x, y); !overflow {
		div256(z, &low, &divisor, roundUp)
		return true
	}
	if _, overflow := z.MulDivOverflow(x, y, &divisor); overflow {
		return false
	}
	if !roundUp {
		return true
	}
	// The remainder x*y - z*d is below d, so it is non-zero exactly when the
	// two products differ modulo 2^256.
	var quotientTimesDivisor uint256.Int
	quotientTimesDivisor.Mul(z, &divisor)
	if low.Eq(&quotientTimesDivisor) {
		return true
	}
	_, overflow := z.AddOverflow(z, u256One)
	return !overflow
}

// div256 sets z to x/d rounded down, or up when roundUp is set. d must be
// non-zero; the rounded-up quotient cannot overflow because a remainder
// implies d > 1. z may alias x.
func div256(z, x, d *uint256.Int, roundUp bool) {
	var remainder uint256.Int
	z.DivMod(x, d, &remainder)
	if roundUp && !remainder.IsZero() {
		z.AddUint64(z, 1)
	}
}

// mul128 returns the 256-bit product of the 128-bit values (aHigh, aLow) and
// (bHigh, bLow) as four 64-bit words, most significant first.
func mul128(aHigh, aLow, bHigh, bLow uint64) (word3, word2, word1, word0 uint64) {
	lowHigh, word0 := bits.Mul64(aLow, bLow)
	crossAHigh, crossALow := bits.Mul64(aLow, bHigh)
	crossBHigh, crossBLow := bits.Mul64(aHigh, bLow)
	highHigh, highLow := bits.Mul64(aHigh, bHigh)

	word1, carryA := bits.Add64(lowHigh, crossALow, 0)
	word1, carryB := bits.Add64(word1, crossBLow, 0)

	word2, carry2A := bits.Add64(crossAHigh, crossBHigh, 0)
	word2, carry2B := bits.Add64(word2, highLow, 0)
	word2, carry2C := bits.Add64(word2, carryA+carryB, 0)
	return highHigh + carry2A + carry2B + carry2C, word2, word1, word0
}

// setU256FromBig sets z to a non-negative v and reports whether it fits
// in maxBits bits.
func setU256FromBig(z *uint256.Int, v *big.Int, maxBits int) bool {
	if v.Sign() < 0 || v.BitLen() > maxBits {
		return false
	}
	z.SetFromBig(v)
	return true
}

const wordsPer256 = 256 / bits.UintSize

// bigResults lets the four integers of a SwapResult share one allocation.
// Each value owns a disjoint, capacity-limited slice of words, so a caller
// that later grows one reallocates instead of writing into its neighbor.
type bigResults struct {
	values [4]big.Int
	words  [4 * wordsPer256]big.Word
}

func (r *bigResults) set(i int, v *uint256.Int) *big.Int {
	words := r.words[i*wordsPer256 : (i+1)*wordsPer256 : (i+1)*wordsPer256]
	for j, limb := range v {
		if bits.UintSize == 64 {
			words[j] = big.Word(limb)
		} else {
			words[2*j] = big.Word(limb)
			words[2*j+1] = big.Word(limb >> 32)
		}
	}
	return r.values[i].SetBits(words)
}
