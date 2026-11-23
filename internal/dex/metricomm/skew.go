package metricomm

import (
	"encoding/binary"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// The pools' quote extension widens one side of the quote after the pool's
// position has moved away from a level the extension tracks. Its source is
// not verified: the rule below was fitted to the deployed contract by calling
// it with overridden storage and block time, and reproduces every sample in
// testdata/quotes.json to the last bit. NextSkewState gives the state word
// after a swap.

// SkewConfig is the part of the quote extension's per-pool configuration the
// widening depends on.
type SkewConfig struct {
	// BinLengthPpm is the width of one bin as a distance from the price.
	BinLengthPpm uint16
	// GainE4 is the widening per unit of excess distance, in 1/10000.
	GainE4 uint16
	// RelaxSeconds is the time the tracked level takes to reach its target.
	RelaxSeconds uint32
}

// DecodeSkewConfig reads the configuration from the extension's storage: the
// first word of the pool's record (oracle address in the low 160 bits, the
// bin length above it) and the word 9 slots further (gain in the low 16
// bits, relaxation time at bit 64).
func DecodeSkewConfig(configWord, parametersWord domain.Hash) SkewConfig {
	return SkewConfig{
		BinLengthPpm: binary.BigEndian.Uint16(configWord[10:12]),
		GainE4:       binary.BigEndian.Uint16(parametersWord[30:32]),
		RelaxSeconds: binary.BigEndian.Uint32(parametersWord[20:24]),
	}
}

// SkewPpm returns the spread the extension adds to the bid and to the ask, in
// ppm of the price, for a swap in a block with the given timestamp. At most
// one of the two is non-zero. Add them to the feed's spreads before Quote.
//
// stateWord is the extension's state for the pool (10 slots after the first
// word of its record): a level in bins as signed Q32.32 in the low 64 bits,
// the level it moves to in the next 64, and the time of the last update in
// the 32 bits above. The level moves linearly to its target over
// RelaxSeconds. Once the pool's position (current bin plus the fraction of
// it already crossed, from slot 0) is more than one bin above the level the
// bid widens, more than one bin below it the ask, by GainE4 of the excess,
// rounded up to a whole ppm.
func SkewPpm(slot0, stateWord domain.Hash, config SkewConfig, blockTime uint64) (bidPpm, askPpm uint32) {
	var pool State
	pool.ApplySlot0(slot0)
	level := int64(binary.BigEndian.Uint64(stateWord[24:32]))
	target := int64(binary.BigEndian.Uint64(stateWord[16:24]))
	updatedAt := uint64(binary.BigEndian.Uint32(stateWord[12:16]))

	// Everything below is in bins scaled by 2^104 * duration, so the
	// interpolation and the position stay exact integers.
	duration := max(uint64(config.RelaxSeconds), 1)
	elapsed := duration
	if blockTime < updatedAt+duration {
		elapsed = max(blockTime, updatedAt) - updatedAt
	}
	moved := new(big.Int).Mul(big.NewInt(target-level), new(big.Int).SetUint64(elapsed))
	current := new(big.Int).Mul(big.NewInt(level), new(big.Int).SetUint64(duration))
	current.Add(current, moved).Lsh(current, 104-32)

	position := new(big.Int).Lsh(big.NewInt(int64(pool.CurBinIdx)), 104)
	position.Add(position, pool.CurPosInBin.ToBig())
	position.Mul(position, new(big.Int).SetUint64(duration))

	oneBin := new(big.Int).Lsh(new(big.Int).SetUint64(duration), 104)
	distance := position.Sub(position, current)
	excess := new(big.Int).Abs(distance)
	excess.Sub(excess, oneBin)
	if excess.Sign() <= 0 {
		return 0, 0
	}
	widening := excess.Mul(excess, big.NewInt(int64(config.GainE4)*int64(config.BinLengthPpm)))
	denominator := new(big.Int).Mul(oneBin, big.NewInt(10_000))
	widening.Add(widening, denominator).Sub(widening, bigOne).Quo(widening, denominator)
	ppm := uint32(1_000_000)
	if widening.IsUint64() && widening.Uint64() < 1_000_000 {
		ppm = uint32(widening.Uint64())
	}
	if distance.Sign() > 0 {
		return ppm, 0
	}
	return 0, ppm
}
