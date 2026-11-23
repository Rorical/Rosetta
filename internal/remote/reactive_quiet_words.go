package remote

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

// A pool's condition words can be derived from its events instead of read. The word that packs the price also packs the oracle's observation
// index and cardinality, which a pool advances on its own:
//
//   - a swap that changes the tick, or a mint or burn of liquidity in range,
//     writes an observation;
//   - a write in a block timestamp that already has one changes nothing;
//   - otherwise the index advances by one modulo the cardinality, which first
//     grows to cardinalityNext when the index sits on its last entry.
//
// Checked against eth_getStorageAt on Arbitrum One: 3,160 blocks
// with events on 47 Uniswap V3, Pancake V3 and Slipstream pools, every derived
// price word equal to the chain's; the liquidity word on 1,701 of 1,701.
//
// The bits above the fields followed here (the protocol fee, the lock) stay
// as read. A word that is wrong only gets the transaction dropped free.
//
// A Uniswap V2 pair and a Uniswap V4 pool are followed the same way, with
// words of their own shape (reactive_quiet_shapes.go), and so is a Camelot V3
// pool (reactive_quiet_camelot.go).

var (
	topicSwap            = topicHash("c42079f94a6350d7e6235f29174924f928cc2ac818eb64fed8004e115fbcca67")
	topicSwapPancake     = topicHash("19b47279256b2a23a1665c810c8d55a1758940ee09377d4f8d26497a3577dc83")
	topicMint            = topicHash("7a53080ba414158be7ec69b987b5fb7d07dee101fe85488f0853ae16239d0bde")
	topicBurn            = topicHash("0c396cd989a39f4459b5fa1aed6a9a8dcdbc45908acfd67e028cd568da98982c")
	topicCardinalityNext = topicHash("ac49e518f90a358f652e4400164f05a5d8f7e35e7747279bc3a93dbf584e125a")
)

// topicHash decodes a constant topic; a malformed one stays zero and matches
// no log.
func topicHash(text string) domain.Hash {
	hash, _ := domain.ParseHash("0x" + text)
	return hash
}

// wordShape is how a pool's condition words are packed and which events move
// them.
type wordShape uint8

const (
	shapeV3      wordShape = iota // slot0 with the oracle's index and cardinality, and the liquidity word
	shapeV4                       // the PoolManager's slot0 and liquidity word of one pool
	shapeV2                       // a pair's reserves word
	shapeCamelot                  // Algebra's globalState with both fees and the timepoint index, and liquidity under the block's volume
)

// shapeOf is the shape of kind's words; false when they are only read
// (Solidly V3's slot0 carries its fee; no Solidly pool is tracked on a chain
// that takes conditions).
func shapeOf(kind domain.PoolKind) (wordShape, bool) {
	switch kind {
	case domain.PoolKindV3, domain.PoolKindSlipstream:
		return shapeV3, true
	case domain.PoolKindV4:
		return shapeV4, true
	case domain.PoolKindV2:
		return shapeV2, true
	case domain.PoolKindCamelotV3:
		return shapeCamelot, true
	}
	return 0, false
}

// derivedKind reports whether kind's words are derived from events.
func derivedKind(kind domain.PoolKind) bool {
	_, ok := shapeOf(kind)
	return ok
}

// wordKey names a model: the pool's address, or for a V4 pool the
// PoolManager's address and the pool's ID.
type wordKey struct {
	address domain.Address
	id      domain.Hash
}

func modelKey(key domain.PoolKey) wordKey {
	if key.IsV4() {
		return wordKey{address: key.Address, id: key.PoolID}
	}
	return wordKey{address: key.Address}
}

// logKey is the model a followed log belongs to: the PoolManager names the
// pool in the first indexed argument of its events.
func logKey(log ingest.Log) wordKey {
	switch log.Topics[0] {
	case topicV4Swap, topicV4Modify, topicV4ProtocolFee:
		if len(log.Topics) > 1 {
			return wordKey{address: log.Address, id: log.Topics[1]}
		}
	}
	return wordKey{address: log.Address}
}

// followedTopic reports whether a log with this first topic can move a
// followed word.
func followedTopic(topic domain.Hash) bool {
	switch topic {
	case topicSwap, topicSwapPancake, topicMint, topicBurn, topicCardinalityNext, topicSync, topicV4Swap, topicV4Modify, topicV4ProtocolFee,
		topicCamelotFee, topicCamelotCommunityFee:
		return true
	}
	return false
}

// wordModel follows one pool's condition words: price and liquidity, or a
// pair's reserves.
type wordModel struct {
	shape                        wordShape
	slots                        []domain.Hash // price, liquidity; or reserves
	price                        *big.Int
	tick                         int32
	index, cardinality, nextCard uint16
	rest                         *big.Int // the price word above bit 232
	liquidity, liquidityHigh     *big.Int // low 128 bits, and the word above them
	// A Camelot pool's two fees; its index is the timepoint index and its
	// liquidityHigh the block's volume per liquidity.
	feeZto, feeOtz uint16
	// last is the timestamp of the newest observation. A word read at a block
	// does not say whether that block's second already has one: unsure marks
	// last as the read block's time, and a write in that same second cannot
	// be derived.
	last   uint64
	unsure bool
	// A V2 pair's word: both reserves and the timestamp of its last update.
	reserve0, reserve1 *big.Int
	stamp              uint32
}

var (
	mask160 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 160), big.NewInt(1))
	mask128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
)

// newWordModel reads the fields out of words of the given shape, read at a
// block with the given timestamp; nil when the words do not have that shape.
func newWordModel(shape wordShape, slots []execution.SlotCondition, timestamp uint64) *wordModel {
	switch shape {
	case shapeV2:
		return newPairModel(slots, timestamp)
	case shapeV4:
		return newV4Model(slots)
	case shapeCamelot:
		return newCamelotModel(slots)
	}
	if len(slots) != 2 {
		return nil
	}
	price, liquidity := new(big.Int).SetBytes(slots[0].Value[:]), new(big.Int).SetBytes(slots[1].Value[:])
	field := func(shift uint, bits uint) uint64 {
		v := new(big.Int).Rsh(price, shift)
		return v.And(v, new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), bits), big.NewInt(1))).Uint64()
	}
	m := &wordModel{slots: []domain.Hash{slots[0].Slot, slots[1].Slot}, last: timestamp, unsure: true}
	m.price = new(big.Int).And(price, mask160)
	m.tick = int32(uint32(field(160, 24))<<8) >> 8
	m.index, m.cardinality, m.nextCard = uint16(field(184, 16)), uint16(field(200, 16)), uint16(field(216, 16))
	m.rest = new(big.Int).Rsh(price, 232)
	m.liquidity = new(big.Int).And(liquidity, mask128)
	m.liquidityHigh = new(big.Int).Rsh(liquidity, 128)
	if m.cardinality == 0 {
		return nil
	}
	return m
}

// conditions are the pool's words as the model has them.
func (m *wordModel) conditions() []execution.SlotCondition {
	switch m.shape {
	case shapeV2:
		return m.pairConditions()
	case shapeV4:
		return m.v4Conditions()
	case shapeCamelot:
		return m.camelotConditions()
	}
	price := new(big.Int).Set(m.price)
	for _, part := range []struct {
		value uint64
		shift uint
	}{{uint64(uint32(m.tick) & 0xFFFFFF), 160}, {uint64(m.index), 184}, {uint64(m.cardinality), 200}, {uint64(m.nextCard), 216}} {
		price.Or(price, new(big.Int).Lsh(new(big.Int).SetUint64(part.value), part.shift))
	}
	price.Or(price, new(big.Int).Lsh(m.rest, 232))
	liquidity := new(big.Int).Or(m.liquidity, new(big.Int).Lsh(m.liquidityHigh, 128))
	out := []execution.SlotCondition{{Slot: m.slots[0]}, {Slot: m.slots[1]}}
	price.FillBytes(out[0].Value[:])
	liquidity.FillBytes(out[1].Value[:])
	return out
}

// write applies the oracle's rule at a block timestamp; false means the
// model can no longer be trusted.
func (m *wordModel) write(timestamp uint64) bool {
	if m.last == timestamp {
		return !m.unsure
	}
	m.unsure = false
	if m.nextCard > m.cardinality && m.index == m.cardinality-1 {
		m.cardinality = m.nextCard
	}
	m.index = uint16((uint32(m.index) + 1) % uint32(m.cardinality))
	m.last = timestamp
	return true
}

// dataWord is the i-th 32-byte word of log data.
func dataWord(data []byte, i int) (*big.Int, bool) {
	if len(data) < 32*(i+1) {
		return nil, false
	}
	return new(big.Int).SetBytes(data[32*i : 32*(i+1)]), true
}

// tick24 reads a sign-extended int24 from a 32-byte word.
func tick24(raw []byte) int32 {
	return int32(uint32(raw[29])<<24|uint32(raw[30])<<16|uint32(raw[31])<<8) >> 8
}

// apply follows one log of the pool; false means the model must be dropped.
func (m *wordModel) apply(log ingest.Log, timestamp uint64) bool {
	if len(log.Topics) == 0 {
		return true
	}
	switch m.shape {
	case shapeV2:
		return m.applyPair(log, timestamp)
	case shapeV4:
		return m.applyV4(log)
	case shapeCamelot:
		return m.applyCamelot(log)
	}
	switch log.Topics[0] {
	case topicSwap, topicSwapPancake:
		price, ok := dataWord(log.Data, 2)
		liquidity, ok2 := dataWord(log.Data, 3)
		if !ok || !ok2 || len(log.Data) < 160 {
			return false
		}
		tick := tick24(log.Data[128:160])
		if tick != m.tick && !m.write(timestamp) {
			return false
		}
		m.price, m.tick, m.liquidity = price, tick, liquidity
	case topicMint, topicBurn:
		at := 0
		if log.Topics[0] == topicMint {
			at = 1 // after the sender
		}
		amount, ok := dataWord(log.Data, at)
		if !ok || len(log.Topics) < 4 {
			return false
		}
		lower, upper := tick24(log.Topics[2][:]), tick24(log.Topics[3][:])
		if amount.Sign() == 0 || m.tick < lower || m.tick >= upper {
			return true
		}
		if !m.write(timestamp) {
			return false
		}
		if log.Topics[0] == topicMint {
			m.liquidity = new(big.Int).Add(m.liquidity, amount)
		} else {
			m.liquidity = new(big.Int).Sub(m.liquidity, amount)
		}
		if m.liquidity.Sign() < 0 || m.liquidity.Cmp(mask128) > 0 {
			return false
		}
	case topicCardinalityNext:
		next, ok := dataWord(log.Data, 1)
		if !ok || !next.IsUint64() || next.Uint64() > 0xFFFF {
			return false
		}
		m.nextCard = uint16(next.Uint64())
	}
	return true
}

// matches reports whether the model is at the pool's state.
func (m *wordModel) matches(state pool.State) bool {
	if m.shape == shapeV2 {
		return state.Reserve0 != nil && state.Reserve1 != nil && m.reserve0.Cmp(state.Reserve0) == 0 && m.reserve1.Cmp(state.Reserve1) == 0
	}
	return state.SqrtPriceX96 != nil && state.Liquidity != nil && m.price.Cmp(state.SqrtPriceX96) == 0 && m.liquidity.Cmp(state.Liquidity) == 0
}
