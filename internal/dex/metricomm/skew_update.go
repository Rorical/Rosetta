package metricomm

import (
	"encoding/binary"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// How the quote extension moves its state word after a swap.
//
// The rule was read out of the deployed bytecode (the afterSwap hook,
// selector 0xe6a7bba0, and the routines it calls) and then checked against
// the chain: testdata/*-skew-updates.json holds real blocks with one or
// several swaps, and replaying their Swap logs through NextSkewState gives
// the extension's storage word after the block bit for bit
// (TestNextSkewStateMatchesChain). Two traced swaps show the write, and the
// event that reports it, inside afterSwap; beforeSwap leaves the word as it
// is, since it is unchanged over every sampled block whose swaps stay in the
// target's bin pair.
//
// Not covered by a sample: a record whose tracking flag is set (neither pool
// has it), a block timestamp at or before the last update other than a
// second update in the same block, and the hook's overflow reverts, which
// need a level outside int64 and cannot happen on a bin grid.

// Layout of the state word. The level is a signed Q32.96 number of bins: its
// upper 64 bits sit at bit 0, the 64 bits below them at bit 176. SkewPpm
// reads only the upper part.
const (
	skewLevelOffset    = 24 // bits 0..63
	skewTargetOffset   = 16 // bits 64..127, signed Q32.32
	skewTimeOffset     = 10 // bits 128..175
	skewFractionOffset = 2  // bits 176..239
	// skewFrozenOffset is the byte of the configuration word (bit 216) that
	// makes afterSwap return before it touches the state.
	skewFrozenOffset = 4
)

// NextSkewState returns the quote extension's state word after a swap that
// left the pool at slot0After, in a block with the given timestamp. Apply it
// once per swap, in log order; stateWord, configWord and parametersWord are
// the words SkewSlots names.
//
// The extension tracks the pool's bin in steps of two: an odd bin index
// counts as the even one above it. While a swap ends in the bin pair the
// target already names, the word is left alone, however far the position
// moved inside the pair. Otherwise the level is first brought to the block's
// time (it moves linearly from its stored value to the target over the
// relaxation time, the division truncating toward zero), the pair's bin
// becomes the new target and the block time the new update time. The swap's
// size, direction and starting position do not enter.
func NextSkewState(stateWord, slot0After, configWord, parametersWord domain.Hash, blockTime uint64) domain.Hash {
	if configWord[skewFrozenOffset] != 0 {
		return stateWord
	}
	bin := int64(int16(binary.BigEndian.Uint16(slot0After[29:31])))
	if bin%2 != 0 {
		bin++
	}
	target := int64(binary.BigEndian.Uint64(stateWord[skewTargetOffset:]))
	// Go's division truncates toward zero like SDIV.
	if target/(1<<32) == bin {
		return stateWord
	}
	relaxSeconds := uint64(binary.BigEndian.Uint32(parametersWord[20:24]))
	level := skewLevelAt(stateWord, relaxSeconds, blockTime)

	var next domain.Hash
	var packed [16]byte
	// Two's complement of the Q32.96 level, truncated to 128 bits: the hook
	// stores level >> 64 (arithmetic) and the 64 bits it shifted out.
	level.And(level, mask128).FillBytes(packed[:])
	copy(next[skewLevelOffset:], packed[:8])
	copy(next[skewFractionOffset:], packed[8:])
	binary.BigEndian.PutUint64(next[skewTargetOffset:], uint64(bin<<32))
	var seconds [8]byte
	binary.BigEndian.PutUint64(seconds[:], blockTime)
	copy(next[skewTimeOffset:skewTargetOffset], seconds[2:])
	return next
}

var mask128 = new(big.Int).Sub(new(big.Int).Lsh(bigOne, 128), bigOne)

// skewLevelAt is the level of a state word at a block time, in bins as
// signed Q32.96, by the extension's own arithmetic.
func skewLevelAt(stateWord domain.Hash, relaxSeconds, blockTime uint64) *big.Int {
	level := big.NewInt(int64(binary.BigEndian.Uint64(stateWord[skewLevelOffset:])))
	level.Lsh(level, 64)
	level.Add(level, new(big.Int).SetUint64(binary.BigEndian.Uint64(stateWord[skewFractionOffset:])))
	target := big.NewInt(int64(binary.BigEndian.Uint64(stateWord[skewTargetOffset:])))
	target.Lsh(target, 64)

	var seconds [8]byte
	copy(seconds[2:], stateWord[skewTimeOffset:skewTargetOffset])
	updatedAt := binary.BigEndian.Uint64(seconds[:])
	if level.Cmp(target) == 0 || updatedAt == 0 || blockTime <= updatedAt {
		return level
	}
	elapsed := blockTime - updatedAt
	if elapsed >= relaxSeconds {
		return target
	}
	moved := target.Sub(target, level)
	moved.Mul(moved, new(big.Int).SetUint64(elapsed))
	// Quo truncates toward zero like SDIV.
	moved.Quo(moved, new(big.Int).SetUint64(relaxSeconds))
	return level.Add(level, moved)
}
