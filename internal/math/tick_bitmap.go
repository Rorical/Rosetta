package math

import (
	"math/big"
	"math/bits"
)

// TickBitmapNextInitialized searches one 256-bit bitmap word at or below tick
// for lte, or strictly above tick otherwise. Missing words are zero.
func TickBitmapNextInitialized(bitmap map[int16]*big.Int, tick, tickSpacing int32, lte bool) (int32, bool) {
	if tickSpacing <= 0 {
		return 0, false
	}
	compressed := floorDiv(tick, tickSpacing)
	if !lte {
		compressed++
	}
	position := int(compressed & 255)
	word := bitmap[int16(compressed>>8)]
	if found, ok := bitmapBit(word, position, lte); ok {
		return (compressed + int32(found-position)) * tickSpacing, true
	}
	boundary := 255
	if lte {
		boundary = 0
	}
	return (compressed + int32(boundary-position)) * tickSpacing, false
}

func bitmapBit(word *big.Int, position int, lte bool) (int, bool) {
	if word == nil {
		return 0, false
	}
	// Preserve low-256-bit two's-complement behavior for malformed negatives.
	// Valid uint256 words take the allocation-free limb scan below.
	var normalized big.Int
	if word.Sign() < 0 {
		word = normalized.And(word, uint256Max)
	}
	limbs := word.Bits()
	index, offset := position/bits.UintSize, uint(position%bits.UintSize)
	if lte {
		for i := min(index, len(limbs)-1); i >= 0; i-- {
			value := uint(limbs[i])
			if i == index {
				value &= ^uint(0) >> (bits.UintSize - 1 - offset)
			}
			if value != 0 {
				return i*bits.UintSize + bits.Len(value) - 1, true
			}
		}
		return 0, false
	}
	for i := index; i < min(len(limbs), 256/bits.UintSize); i++ {
		value := uint(limbs[i])
		if i == index {
			value &= ^uint(0) << offset
		}
		if value != 0 {
			return i*bits.UintSize + bits.TrailingZeros(value), true
		}
	}
	return 0, false
}
