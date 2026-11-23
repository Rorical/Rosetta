package metricomm

import (
	"errors"
	"fmt"
	"slices"

	"github.com/holiman/uint256"
	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ErrInvalidLog reports a swap log that does not fit the storage words it is
// applied to: a payload of the wrong size, a bin outside the grid, or a
// balance that leaves its field. The words are then out of step with the
// pool and have to be read again.
var ErrInvalidLog = errors.New("metricomm: invalid log")

// Topic0 of the two events a swap emits. A swap logs one BinSwapped per bin
// whose balances moved, then one Swap.
var (
	// TopicBinSwapped is BinSwapped(int256 binIdx, Packed2Int128 binDeltas,
	// Packed2Uint128 fees). binDeltas carries the token0 delta of the bin in
	// its high half and the token1 delta in its low half, both scaled.
	TopicBinSwapped = eventTopic("BinSwapped(int256,uint256,uint256)")
	// TopicSwap is Swap(address indexed sender, address indexed recipient,
	// PackedSwapDetails details, Packed2Int128 amountDeltas, Packed2Uint128
	// platformFees). details packs the position after the swap: posInBin in
	// bits 0..103, binIdx in bits 104..119, the exact-input flag in bit 120.
	TopicSwap = eventTopic("Swap(address,address,uint256,uint256,uint256)")
)

// staleTopics are the pool's other events. Each one changes, or may change,
// something a quote depends on (bin balances and fees, slot 0, the price
// provider) in a way the log alone does not pin down, so the words are read
// again instead. Fee collection leaves the words alone on the pools sampled
// so far, but it is rare enough that a second read costs nothing.
var staleTopics = []domain.Hash{
	eventTopic("LiquidityAdded(address,uint80,int256[],(int256,int256)[],uint256[])"),
	eventTopic("LiquidityRemoved(address,uint80,int256[],(int256,int256)[],uint256[])"),
	eventTopic("ProtocolFeesCollected(uint256,uint256,uint256,uint256)"),
	eventTopic("FeeTransferFailed(address,address,uint256)"),
	eventTopic("SpreadFeeUpdated(uint24)"),
	eventTopic("NotionalFeeUpdated(uint24)"),
	eventTopic("PauseLevelUpdated(uint8,uint8)"),
	eventTopic("PriceProviderUpdated(address)"),
	eventTopic("BinAdditionalFeesUpdated(int16,uint16,uint16)"),
	eventTopic("BidAskRefAsymmetryUpdated(uint16)"),
}

func eventTopic(signature string) domain.Hash {
	hasher := sha3.NewLegacyKeccak256()
	hasher.Write([]byte(signature))
	var topic domain.Hash
	hasher.Sum(topic[:0])
	return topic
}

// Bit layout of slot 0 and of a bin word; see state.go.
const (
	slot0BinIdxShift = 8
	slot0PosShift    = 24
	slot0DistShift   = 128
	binToken1Shift   = 104
)

var (
	mask16 = uint256.NewInt(0xffff)
	mask24 = uint256.NewInt(0xffffff)
)

// ApplyLog applies one log emitted by the pool to its storage words, which
// are ordered as StorageSlots. After the BinSwapped and Swap logs of a
// transaction the returned words equal the pool's storage after it.
//
// changed reports that next differs from words; next is then a new slice and
// words is left untouched. stale reports an event the replay does not model:
// the words must be read from storage again before the pool is quoted.
// Topics of other contracts' events are ignored. On an error the words no
// longer follow the pool and the caller should treat them as stale too.
func ApplyLog(
	words []domain.Hash, immutables Immutables, topics []domain.Hash, data []byte,
) (next []domain.Hash, changed, stale bool, err error) {
	if len(topics) == 0 {
		return words, false, false, nil
	}
	switch topics[0] {
	case TopicBinSwapped:
		next, err = applyBinSwapped(words, immutables, data)
	case TopicSwap:
		next, err = applySwap(words, immutables, data)
	default:
		return words, false, slices.Contains(staleTopics, topics[0]), nil
	}
	if err != nil {
		return words, false, false, err
	}
	return next, true, false, nil
}

// logWords splits a log payload of exactly count 32-byte words.
func logWords(name string, data []byte, count int) ([]*word, error) {
	if len(data) != 32*count {
		return nil, fmt.Errorf("%w: %s payload of %d bytes, want %d", ErrInvalidLog, name, len(data), 32*count)
	}
	values := make([]*word, count)
	for i := range values {
		values[i] = new(word).SetBytes32(data[32*i : 32*i+32])
	}
	return values, nil
}

func checkWordCount(words []domain.Hash, immutables Immutables) error {
	want := 2 + int(immutables.HighestBin) - int(immutables.LowestBin) + 1
	if want <= 2 || len(words) != want {
		return fmt.Errorf("%w: %d storage words, want %d for grid %d..%d",
			ErrInvalidState, len(words), want, immutables.LowestBin, immutables.HighestBin)
	}
	return nil
}

// signedHalves splits a Packed2Int128 into its sign-extended halves.
func signedHalves(packed *word) (high, low *word) {
	high = new(word).Rsh(packed, 128)
	low = new(word).And(packed, maxUint128)
	return high.ExtendSign(high, uint256.NewInt(15)), low.ExtendSign(low, uint256.NewInt(15))
}

// smallInt256 reads a two's-complement word whose value fits an int16.
func smallInt256(value *word) (int, bool) {
	magnitude := new(word).Abs(value)
	if !magnitude.IsUint64() || magnitude.Uint64() > 1<<15 {
		return 0, false
	}
	if value.Sign() < 0 {
		return -int(magnitude.Uint64()), true
	}
	return int(magnitude.Uint64()), magnitude.Uint64() < 1<<15
}

// addDelta adds a two's-complement delta to an unsigned field that must stay
// below limit.
func addDelta(name string, value, delta, limit *word) (*word, error) {
	sum := add(value, delta)
	if sum.Gt(limit) {
		return nil, fmt.Errorf("%w: %s leaves its range", ErrInvalidLog, name)
	}
	return sum, nil
}

// applyBinSwapped moves one bin's balances and the bin totals by the deltas
// of the event. The totals move by the same amounts: the pool adds the input
// net of platform fees, which is the sum of the per-bin credits.
func applyBinSwapped(words []domain.Hash, immutables Immutables, data []byte) ([]domain.Hash, error) {
	if err := checkWordCount(words, immutables); err != nil {
		return nil, err
	}
	fields, err := logWords("BinSwapped", data, 3)
	if err != nil {
		return nil, err
	}
	binIdx, ok := smallInt256(fields[0])
	if !ok || binIdx < int(immutables.LowestBin) || binIdx > int(immutables.HighestBin) {
		return nil, fmt.Errorf("%w: BinSwapped for a bin outside grid %d..%d", ErrInvalidLog, immutables.LowestBin, immutables.HighestBin)
	}
	position := 2 + binIdx - int(immutables.LowestBin)
	delta0, delta1 := signedHalves(fields[1])

	bin := new(word).SetBytes32(words[position][:])
	token0, err := addDelta("bin token0 balance", new(word).And(bin, maxPosBin), delta0, maxPosBin)
	if err != nil {
		return nil, err
	}
	token1, err := addDelta("bin token1 balance", new(word).And(new(word).Rsh(bin, binToken1Shift), maxPosBin), delta1, maxPosBin)
	if err != nil {
		return nil, err
	}
	totals := new(word).SetBytes32(words[binTotalsIndex][:])
	total0, err := addDelta("token0 total", new(word).And(totals, maxUint128), delta0, maxUint128)
	if err != nil {
		return nil, err
	}
	total1, err := addDelta("token1 total", new(word).Rsh(totals, 128), delta1, maxUint128)
	if err != nil {
		return nil, err
	}

	next := slices.Clone(words)
	setField(bin, 0, maxPosBin, token0)
	setField(bin, binToken1Shift, maxPosBin, token1)
	next[position] = bin.Bytes32()
	next[binTotalsIndex] = total0.Or(total0, total1.Lsh(total1, 128)).Bytes32()
	return next, nil
}

// applySwap writes the position after a swap into slot 0. The event carries
// the bin and the position inside it; the distance of that bin from the
// reference price is rebuilt from the lengths of the bins the swap crossed,
// exactly as _swapAcrossBins steps it.
func applySwap(words []domain.Hash, immutables Immutables, data []byte) ([]domain.Hash, error) {
	if err := checkWordCount(words, immutables); err != nil {
		return nil, err
	}
	fields, err := logWords("Swap", data, 3)
	if err != nil {
		return nil, err
	}
	details := fields[0]
	if !new(word).Rsh(details, 121).IsZero() {
		return nil, fmt.Errorf("%w: Swap details use bits above 120", ErrInvalidLog)
	}
	position := new(word).And(details, maxPosBin)
	binIdx := int(int16(uint16(new(word).Rsh(details, 104).Uint64())))
	lowest, highest := int(immutables.LowestBin), int(immutables.HighestBin)
	if binIdx < lowest || binIdx > highest {
		return nil, fmt.Errorf("%w: Swap ends in bin %d outside grid %d..%d", ErrInvalidLog, binIdx, lowest, highest)
	}

	slot0 := new(word).SetBytes32(words[slot0Index][:])
	current := int(int16(uint16(new(word).Rsh(slot0, slot0BinIdxShift).Uint64())))
	if current < lowest || current > highest {
		return nil, fmt.Errorf("%w: current bin %d outside grid %d..%d", ErrInvalidState, current, lowest, highest)
	}
	// Sign-extend the int24 through the top byte of an int32.
	distance := int64(int32(uint32(new(word).Rsh(slot0, slot0DistShift).Uint64())<<8) >> 8)
	binLength := func(idx int) int64 {
		return int64(DecodeBin(words[2+idx-lowest]).LengthE6)
	}
	// Moving down subtracts the length of each bin entered. Moving up adds
	// the length of each bin left and clamps to int24 at every step.
	for idx := current - 1; idx >= binIdx; idx-- {
		distance -= binLength(idx)
	}
	for idx := current; idx < binIdx; idx++ {
		distance = min(max(distance+binLength(idx), minInt24), maxInt24)
	}
	if distance < minInt24 || distance > maxInt24 {
		// _finalizeSwap narrows with a checked cast, so the swap would have reverted.
		return nil, fmt.Errorf("%w: Swap moves the bin distance outside int24", ErrInvalidLog)
	}

	setField(slot0, slot0BinIdxShift, mask16, uint256.NewInt(uint64(uint16(int16(binIdx)))))
	setField(slot0, slot0PosShift, maxPosBin, position)
	setField(slot0, slot0DistShift, mask24, uint256.NewInt(uint64(uint32(int32(distance))&0xffffff)))
	next := slices.Clone(words)
	next[slot0Index] = slot0.Bytes32()
	return next, nil
}

// setField overwrites the bits of packed selected by mask<<shift with value.
func setField(packed *word, shift uint, mask, value *word) {
	cleared := new(word).Not(new(word).Lsh(mask, shift))
	packed.And(packed, cleared)
	packed.Or(packed, new(word).Lsh(new(word).And(value, mask), shift))
}
