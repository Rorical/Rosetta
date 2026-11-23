package remote

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
)

// The words of a Uniswap V2 pair and of a Uniswap V4 pool, derived from their
// events like the V3 words (reactive_quiet_words.go). Both were replayed
// against eth_getStorageAt on Arbitrum One (the runs are the
// fixtures under testdata/quiet-words):
//
//   - V2, pairs of the Uniswap and Sushi factories: the word after every
//     block with events equalled reserve0 | reserve1 << 112 | timestamp << 224
//     of the block's last Sync, in 206 of 206 blocks on 5 pairs, among them a
//     mint, a burn and a block with two Syncs. A block with only transfers of
//     the pair's own token left the word as it was.
//   - V4, hookless pools of the one PoolManager: slot0 and the liquidity word
//     in 302 of 302 blocks on 4 pools, with 418 Swaps and 25 ModifyLiquidity,
//     in and out of the current range.
//
// A V4 Donate is not followed: it adds to the fee growth words only. The five
// Donates in 10,000 blocks all came right after a Swap of the same pool, and
// the words after the block were the Swap's.

var (
	topicSync = topicHash("1c411e9a96e071241c2f21f7726b17ae89e3cab4c78be50e062b03a9fffbbad1")
	// PoolManager events; the pool's ID is their first indexed argument.
	topicV4Swap   = topicHash("40e9cecb9f5f1f1c5b9c97dec2917b7ee92e57ba5563708daca94dd84ad7112f")
	topicV4Modify = topicHash("f208f4912782fd25c7f114ca3723a2d5dd6f3bcc3ac8db5af63baa85f711d5ec")
	// ProtocolFeeUpdated(bytes32,uint24). UNVERIFIED on chain: the topic is
	// the hash of the signature in v4-core's IProtocolFees and no such log
	// was in the measured blocks. One log of it on Arbitrum One would confirm.
	topicV4ProtocolFee = topicHash("e9c42593e71f84403b84352cd168d693e2c9fcd1fdbcc3feb21d92b43e6696f9")
)

var (
	mask112 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 112), big.NewInt(1))
	mask24  = big.NewInt(0xFFFFFF)
)

// pairEpoch is the timestamp of Uniswap V2's deployment (2020-05-04). No pair
// was updated before it, so a word whose top 32 bits are lower does not carry
// the time of its last update there: a fork that packs something else above
// the reserves (Camelot's pair keeps two fee percents) is not followed.
const pairEpoch = 1588550400

// newPairModel reads a V2 pair's reserves word, read at a block with the given
// timestamp. The reserves were matched against our state before (holdsState);
// here the top 32 bits must be a time the pair could last have been updated.
func newPairModel(slots []execution.SlotCondition, timestamp uint64) *wordModel {
	if len(slots) != 1 {
		return nil
	}
	word := new(big.Int).SetBytes(slots[0].Value[:])
	stamp := uint32(new(big.Int).Rsh(word, 224).Uint64())
	if stamp < pairEpoch || stamp > uint32(timestamp) {
		return nil
	}
	return &wordModel{shape: shapeV2, slots: []domain.Hash{slots[0].Slot}, stamp: stamp,
		reserve0: new(big.Int).And(word, mask112), reserve1: new(big.Int).And(new(big.Int).Rsh(word, 112), mask112)}
}

func (m *wordModel) pairConditions() []execution.SlotCondition {
	word := new(big.Int).Lsh(new(big.Int).SetUint64(uint64(m.stamp)), 224)
	word.Or(word, new(big.Int).Lsh(m.reserve1, 112)).Or(word, m.reserve0)
	out := []execution.SlotCondition{{Slot: m.slots[0]}}
	word.FillBytes(out[0].Value[:])
	return out
}

// applyPair follows a pair's Sync: _update stores both reserves and the
// block's timestamp, truncated to 32 bits, and emits Sync, on every mint,
// burn, swap and sync. skim calls no _update and emits no Sync.
func (m *wordModel) applyPair(log ingest.Log, timestamp uint64) bool {
	if log.Topics[0] != topicSync {
		return true
	}
	reserve0, ok := dataWord(log.Data, 0)
	reserve1, ok2 := dataWord(log.Data, 1)
	if !ok || !ok2 || reserve0.Cmp(mask112) > 0 || reserve1.Cmp(mask112) > 0 {
		return false
	}
	m.reserve0, m.reserve1, m.stamp = reserve0, reserve1, uint32(timestamp)
	return true
}

// stampedByLastSync checks a pair model started from words read at the ring's
// block at: when the ring holds a Sync of the pair up to that block, the word
// must carry that Sync's block time. The caller holds mu.
func (q *quietPools) stampedByLastSync(m *wordModel, key wordKey, at int) bool {
	for i := at; i >= 0; i-- {
		for _, log := range q.recent[i].logs {
			if log.Topics[0] == topicSync && logKey(log) == key {
				return m.stamp == uint32(q.recent[i].timestamp)
			}
		}
	}
	return true
}

// syncedAfter reports whether the ring holds a Sync of the pair after block,
// or does not reach back to block. The caller holds mu.
func (q *quietPools) syncedAfter(key wordKey, block uint64) bool {
	if len(q.recent) == 0 || q.recent[0].number > block {
		return true
	}
	for _, later := range q.recent {
		if later.number <= block {
			continue
		}
		for _, log := range later.logs {
			if log.Topics[0] == topicSync && logKey(log) == key {
				return true
			}
		}
	}
	return false
}

// newV4Model reads a V4 pool's slot0 and liquidity words. slot0 packs the
// price, the tick above it, and from bit 184 the protocol fee and the LP fee,
// which stay as read.
func newV4Model(slots []execution.SlotCondition) *wordModel {
	if len(slots) != 2 {
		return nil
	}
	price, liquidity := new(big.Int).SetBytes(slots[0].Value[:]), new(big.Int).SetBytes(slots[1].Value[:])
	if new(big.Int).And(price, mask160).Sign() == 0 || price.BitLen() > 232 {
		return nil // not initialized, or not a V4 slot0
	}
	tick := new(big.Int).And(new(big.Int).Rsh(price, 160), mask24)
	return &wordModel{shape: shapeV4, slots: []domain.Hash{slots[0].Slot, slots[1].Slot},
		price: new(big.Int).And(price, mask160), tick: int32(uint32(tick.Uint64())<<8) >> 8, rest: new(big.Int).Rsh(price, 184),
		liquidity: new(big.Int).And(liquidity, mask128), liquidityHigh: new(big.Int).Rsh(liquidity, 128)}
}

func (m *wordModel) v4Conditions() []execution.SlotCondition {
	price := new(big.Int).Lsh(m.rest, 184)
	price.Or(price, new(big.Int).Lsh(new(big.Int).SetUint64(uint64(uint32(m.tick)&0xFFFFFF)), 160)).Or(price, m.price)
	liquidity := new(big.Int).Or(m.liquidity, new(big.Int).Lsh(m.liquidityHigh, 128))
	out := []execution.SlotCondition{{Slot: m.slots[0]}, {Slot: m.slots[1]}}
	price.FillBytes(out[0].Value[:])
	liquidity.FillBytes(out[1].Value[:])
	return out
}

// applyV4 follows one PoolManager log of the pool; false means the model must
// be dropped. Only hookless pools with a static fee are tracked (discovery
// rejects a pool with hooks, and the catalog's tiers are static), so slot0's
// LP fee never changes; the protocol fee changes with ProtocolFeeUpdated,
// which drops the model for the next read.
func (m *wordModel) applyV4(log ingest.Log) bool {
	switch log.Topics[0] {
	case topicV4Swap:
		// amount0, amount1, sqrtPriceX96, liquidity, tick, fee
		price, ok := dataWord(log.Data, 2)
		liquidity, ok2 := dataWord(log.Data, 3)
		fee, ok3 := dataWord(log.Data, 5)
		if !ok || !ok2 || !ok3 || price.Cmp(mask160) > 0 || liquidity.Cmp(mask128) > 0 || !m.chargesFee(fee) {
			return false
		}
		m.price, m.tick, m.liquidity = price, tick24(log.Data[128:160]), liquidity
	case topicV4Modify:
		// tickLower, tickUpper, liquidityDelta, salt
		delta, ok := dataWord(log.Data, 2)
		if !ok {
			return false
		}
		if delta.Bit(255) == 1 {
			delta.Sub(delta, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		lower, upper := tick24(log.Data[0:32]), tick24(log.Data[32:64])
		if delta.Sign() == 0 || m.tick < lower || m.tick >= upper {
			return true
		}
		m.liquidity = delta.Add(delta, m.liquidity)
		if m.liquidity.Sign() < 0 || m.liquidity.Cmp(mask128) > 0 {
			return false
		}
	case topicV4ProtocolFee:
		return false
	}
	return true
}

// chargesFee reports whether a Swap's fee is the one slot0's fee fields give:
// the LP fee, or with a protocol fee p for the swap's direction (12 bits
// each), p + lp - p*lp/1e6. A swap that charged anything else says a fee field
// changed without an event followed here. On the fixtures every Swap charged
// 625 (p 125, lp 500) or 3499 (p 500, lp 3000); both directions had the same
// p, so either direction's fee is accepted.
func (m *wordModel) chargesFee(fee *big.Int) bool {
	if !fee.IsUint64() {
		return false
	}
	fields := m.rest.Uint64()
	lp := fields >> 24 & 0xFFFFFF
	for _, protocol := range []uint64{fields & 0xFFF, fields >> 12 & 0xFFF} {
		if fee.Uint64() == protocol+lp-protocol*lp/1_000_000 {
			return true
		}
	}
	return false
}
