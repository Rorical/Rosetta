package remote

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
)

// The words of a Camelot V3 pool (Algebra V1.9-directional-fee), derived from
// its events like the V3 words (reactive_quiet_words.go). The layout and the
// write rules are those of the verified source of pool
// 0x299c7d6f2ef82cb52b2ab83b14f05c6b2b803aba and of its DataStorageOperator
// (Sourcify, chain 42161, exact matches):
//
//   - slot 2 is globalState: price (160 bits), tick (24), feeZto (16), feeOtz
//     (16), timepointIndex (16), communityFeeToken0 (8), communityFeeToken1
//     (8), unlocked (8). Slot 3 packs liquidity (128 bits) under
//     volumePerLiquidityInBlock (128).
//   - Every swap, and every mint or burn of liquidity in range, first asks the
//     operator to write a timepoint. The operator writes only when the last
//     timepoint is of another second; the index then advances by one modulo
//     65536, the pool stores the two fees the operator computes, emits
//     Fee(feeZto, feeOtz) whether or not they changed, and clears
//     volumePerLiquidityInBlock. So a Fee log is the write: no block time is
//     needed, and the fee is read from the log, not recomputed.
//   - A swap then stores its price, tick and liquidity and adds
//     calculateVolumePerLiquidity(liquidity, amount0, amount1) to the volume
//     word, all four of them in its Swap log.
//   - A mint or burn in range adds or removes its amount of liquidity.
//   - flash, collect, setTickSpacing, setIncentive and setLiquidityCooldown
//     write neither word. setCommunityFee writes the two community fees.
//
// Replayed against eth_getStorageAt on Arbitrum One (the runs are
// the camelot-*.json fixtures under testdata/quiet-words): both words equal to the
// chain's after 373 of 373 blocks with events on 6 pools of the Camelot
// factory, with 390 Swaps, 350 Fee logs (17 with a new fee), 23 mints and
// burns in range and 20 out of range.

var (
	topicCamelotFee = topicHash("8a89de70856bccec096661388f305b9a75f5f65cb0d8a0e1e803c39dabedb57f")
	// CommunityFee(uint8,uint8). UNVERIFIED on chain: the topic is the hash of
	// the signature in the verified IAlgebraPoolEvents and no such log was in
	// the measured blocks. One log of it on a Camelot pool would confirm.
	topicCamelotCommunityFee = topicHash("9e22b964b08e25c3aaa72102bb0071c089258fb82d51271a8ddf5c24921356ee")
)

var (
	mask16 = big.NewInt(0xFFFF)
	// maxVolumePerLiquidity is the operator's MAX_VOLUME_PER_LIQUIDITY.
	maxVolumePerLiquidity = new(big.Int).Lsh(big.NewInt(100000), 64)
	max256                = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

// newCamelotModel reads globalState and the liquidity word. A pool at rest is
// unlocked, and that flag is the word's top byte only in this layout: the
// six-field globalState of an older Algebra pool has it at bit 232 and gets
// no model.
func newCamelotModel(slots []execution.SlotCondition) *wordModel {
	if len(slots) != 2 {
		return nil
	}
	global, liquidity := new(big.Int).SetBytes(slots[0].Value[:]), new(big.Int).SetBytes(slots[1].Value[:])
	if new(big.Int).And(global, mask160).Sign() == 0 || new(big.Int).Rsh(global, 248).Cmp(big.NewInt(1)) != 0 {
		return nil
	}
	field := func(shift uint, mask *big.Int) uint64 {
		v := new(big.Int).Rsh(global, shift)
		return v.And(v, mask).Uint64()
	}
	return &wordModel{shape: shapeCamelot, slots: []domain.Hash{slots[0].Slot, slots[1].Slot},
		price: new(big.Int).And(global, mask160), tick: int32(uint32(field(160, mask24))<<8) >> 8,
		feeZto: uint16(field(184, mask16)), feeOtz: uint16(field(200, mask16)), index: uint16(field(216, mask16)),
		rest:      new(big.Int).Rsh(global, 232),
		liquidity: new(big.Int).And(liquidity, mask128), liquidityHigh: new(big.Int).Rsh(liquidity, 128)}
}

func (m *wordModel) camelotConditions() []execution.SlotCondition {
	global := new(big.Int).Lsh(m.rest, 232)
	for _, part := range []struct {
		value uint64
		shift uint
	}{{uint64(uint32(m.tick) & 0xFFFFFF), 160}, {uint64(m.feeZto), 184}, {uint64(m.feeOtz), 200}, {uint64(m.index), 216}} {
		global.Or(global, new(big.Int).Lsh(new(big.Int).SetUint64(part.value), part.shift))
	}
	global.Or(global, m.price)
	liquidity := new(big.Int).Or(m.liquidity, new(big.Int).Lsh(m.liquidityHigh, 128))
	out := []execution.SlotCondition{{Slot: m.slots[0]}, {Slot: m.slots[1]}}
	global.FillBytes(out[0].Value[:])
	liquidity.FillBytes(out[1].Value[:])
	return out
}

// applyCamelot follows one log of the pool; false means the model must be
// dropped. The pool emits Fee before the Swap, Mint or Burn of the same call,
// so the logs are applied in their order.
func (m *wordModel) applyCamelot(log ingest.Log) bool {
	switch log.Topics[0] {
	case topicCamelotFee:
		feeZto, ok := dataWord(log.Data, 0)
		feeOtz, ok2 := dataWord(log.Data, 1)
		if !ok || !ok2 || feeZto.Cmp(mask16) > 0 || feeOtz.Cmp(mask16) > 0 {
			return false
		}
		m.feeZto, m.feeOtz = uint16(feeZto.Uint64()), uint16(feeOtz.Uint64())
		m.index++ // modulo 65536, as the operator's uint16
		m.liquidityHigh = new(big.Int)
	case topicSwap:
		// amount0, amount1, price, liquidity, tick
		amount0, ok := dataWord(log.Data, 0)
		amount1, ok2 := dataWord(log.Data, 1)
		price, ok3 := dataWord(log.Data, 2)
		liquidity, ok4 := dataWord(log.Data, 3)
		if !ok || !ok2 || !ok3 || !ok4 || len(log.Data) < 160 || price.Cmp(mask160) > 0 || liquidity.Cmp(mask128) > 0 {
			return false
		}
		m.price, m.tick, m.liquidity = price, tick24(log.Data[128:160]), liquidity
		volume := new(big.Int).Add(m.liquidityHigh, camelotVolumePerLiquidity(liquidity, amount0, amount1))
		m.liquidityHigh = volume.And(volume, mask128) // unchecked uint128 addition (Solidity 0.7.6)
	case topicMint, topicBurn:
		at := 0
		if log.Topics[0] == topicMint {
			at = 1 // after the sender
		}
		amount, ok := dataWord(log.Data, at)
		if !ok || len(log.Topics) < 4 {
			return false
		}
		bottom, top := tick24(log.Topics[2][:]), tick24(log.Topics[3][:])
		if amount.Sign() == 0 || m.tick < bottom || m.tick >= top {
			return true
		}
		if log.Topics[0] == topicMint {
			m.liquidity = new(big.Int).Add(m.liquidity, amount)
		} else {
			m.liquidity = new(big.Int).Sub(m.liquidity, amount)
		}
		if m.liquidity.Sign() < 0 || m.liquidity.Cmp(mask128) > 0 {
			return false
		}
	case topicCamelotCommunityFee:
		// Rewrites the bits kept as read; the next read starts a new model.
		return false
	}
	return true
}

// camelotVolumePerLiquidity is the operator's calculateVolumePerLiquidity:
// sqrt(|amount0|) * sqrt(|amount1|) * 2^64 / liquidity, capped. The amounts
// are the Swap log's int256 words; Sqrt.sqrtAbs is the floor square root (390
// of 390 swaps of the fixtures gave the chain's volume word).
func camelotVolumePerLiquidity(liquidity, amount0, amount1 *big.Int) *big.Int {
	root := func(word *big.Int) *big.Int {
		value := new(big.Int).Set(word)
		if value.Bit(255) == 1 {
			value.Sub(new(big.Int).Lsh(big.NewInt(1), 256), value)
		}
		return value.Sqrt(value)
	}
	volume := root(amount0)
	volume.Mul(volume, root(amount1))
	divisor := liquidity
	if divisor.Sign() == 0 {
		divisor = big.NewInt(1)
	}
	if volume.BitLen() > 192 {
		volume.Set(max256)
	} else {
		volume.Lsh(volume, 64)
	}
	volume.Quo(volume, divisor)
	if volume.Cmp(maxVolumePerLiquidity) > 0 {
		return new(big.Int).Set(maxVolumePerLiquidity)
	}
	return volume
}
