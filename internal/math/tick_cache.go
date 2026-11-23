package math

import (
	"fmt"
	"math/big"
	"sync/atomic"

	"github.com/holiman/uint256"
)

// Direct mapping bounds retained prices across pools, state roots and chains.
// Mix high tick bits into the index: bitmap boundaries otherwise collide at
// every multiple of 4096. Entries are immutable after publication.
var tickPrices [4096]atomic.Pointer[cachedTickPrice]

type cachedTickPrice struct {
	tick  int32
	price *big.Int
}

// TickToSqrtPriceX96 returns sqrt(1.0001^tick) * 2^96 rounded up, matching
// Uniswap V3 TickMath.getSqrtRatioAtTick for ticks in [MinTick, MaxTick].
// Mutating the result cannot modify the shared mathematical lookup table.
func TickToSqrtPriceX96(tick int32) (*big.Int, error) {
	price, err := tickPrice(tick)
	if err != nil {
		return nil, err
	}
	return new(big.Int).Set(price), nil
}

// tickPrice returns an immutable value for internal arithmetic operands only.
func tickPrice(tick int32) (*big.Int, error) {
	if tick < MinTick || tick > MaxTick {
		return nil, fmt.Errorf("%w: %d", ErrTickOutOfRange, tick)
	}
	index := uint32(tick) * 2654435761
	slot := &tickPrices[index>>20]
	if entry := slot.Load(); entry != nil && entry.tick == tick {
		return entry.price, nil
	}
	price, err := calculateTickPrice(tick)
	if err != nil {
		return nil, err
	}
	slot.Store(&cachedTickPrice{tick: tick, price: price})
	return price, nil
}

// tickRatios caches sqrtRatioAtTick for the fixed-width swap, which asks for
// one price per crossed tick and two per loaded pool state. Uncached, that is
// about 15% of CPU. 65,536 slots
// hold the ticks near the price of a few thousand pools.
var tickRatios [1 << 16]atomic.Pointer[cachedTickRatio]

type cachedTickRatio struct {
	tick  int32
	ratio uint256.Int
}

// cachedSqrtRatioAtTick sets z to sqrtRatioAtTick(tick) for a tick in
// [MinTick, MaxTick].
func cachedSqrtRatioAtTick(z *uint256.Int, tick int32) {
	slot := &tickRatios[(uint32(tick)*2654435761)>>16]
	if entry := slot.Load(); entry != nil && entry.tick == tick {
		*z = entry.ratio
		return
	}
	entry := &cachedTickRatio{tick: tick}
	sqrtRatioAtTick(&entry.ratio, tick)
	slot.Store(entry)
	*z = entry.ratio
}
