package discovery

import (
	"slices"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"golang.org/x/crypto/sha3"
)

// Activity counts actual Swap events, not Sync/Mint events. It is sampled from
// canonical receipts; prefixes do not inflate persisted market statistics.
type Activity struct {
	Swaps          uint64
	LastBlock      uint64
	ProfitableUses uint64
	MultiSwapUses  uint64
}
type ActivityBook struct {
	observed   domain.BlockRef
	buckets    map[domain.PoolKey]map[uint64]uint64
	profitable map[domain.PoolKey]uint64
	multiSwap  map[domain.PoolKey]uint64
	unknown    map[domain.PoolKey]bool
	last       map[domain.PoolKey]uint64
	span       uint64
}

var swapV2 = eventHash("Swap(address,uint256,uint256,uint256,uint256,address)")
var swapV3 = eventHash("Swap(address,address,int256,int256,uint160,uint128,int24)")
var swapV4 = eventHash("Swap(bytes32,address,int128,int128,uint160,uint128,int24,uint24)")
var swapVelodrome = eventHash("Swap(address,address,uint256,uint256,uint256,uint256)")

func eventHash(s string) (hash domain.Hash) {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(s))
	copy(hash[:], h.Sum(nil))
	return
}
func (b *ActivityBook) Observe(ref domain.BlockRef, logs []rpc.Log, keys []domain.PoolKey, window uint64) {
	if b.observed.Hash != (domain.Hash{}) && ref.Number <= b.observed.Number {
		return
	}
	b.observed = ref
	if b.buckets == nil {
		b.buckets = map[domain.PoolKey]map[uint64]uint64{}
		b.unknown = map[domain.PoolKey]bool{}
		b.last = map[domain.PoolKey]uint64{}
		b.multiSwap = map[domain.PoolKey]uint64{}
	}
	// At most 129 time bins per emitter; the oldest bin can include up to
	// span-1 blocks before the requested window. Counts are selection hints.
	span := max(uint64(1), window/128+min(window%128, 1))
	if span != b.span {
		for key, counts := range b.buckets {
			grouped := map[uint64]uint64{}
			for number, count := range counts {
				grouped[number/span*span] += count
			}
			b.buckets[key] = grouped
		}
		b.span = span
	}
	addresses := map[domain.Address]domain.PoolKey{}
	v4 := map[domain.Hash]domain.PoolKey{}
	for _, key := range keys {
		unknownKey := key
		if !key.IsV4() {
			unknownKey.Kind = 0
		}
		if b.unknown[unknownKey] {
			if unknownKey != key {
				if b.buckets[key] == nil {
					b.buckets[key] = map[uint64]uint64{}
				}
				for number, count := range b.buckets[unknownKey] {
					b.buckets[key][number] += count
				}
				b.last[key] = max(b.last[key], b.last[unknownKey])
				b.multiSwap[key] += b.multiSwap[unknownKey]
				delete(b.multiSwap, unknownKey)
				delete(b.buckets, unknownKey)
				delete(b.last, unknownKey)
			}
			delete(b.unknown, unknownKey)
		}
		if key.IsV4() {
			v4[key.PoolID] = key
		} else {
			addresses[key.Address] = key
		}
	}
	floor := uint64(0)
	if ref.Number > window {
		floor = ref.Number - window
	}
	for key, buckets := range b.buckets {
		for number := range buckets {
			if number < floor && floor-number >= span {
				delete(buckets, number)
			}
		}
		if len(buckets) == 0 {
			delete(b.buckets, key)
			delete(b.last, key)
			delete(b.unknown, key)
			delete(b.multiSwap, key)
		}
	}
	routed := map[domain.Hash]map[domain.PoolKey]bool{}
	for _, log := range logs {
		if len(log.Topics) == 0 {
			continue
		}
		topic := log.Topics[0]
		if topic != swapV2 && topic != swapV3 && topic != uniswapv3.TopicPancakeSwap && topic != swapV4 && topic != swapVelodrome {
			continue
		}
		key, ok := addresses[log.Address]
		if topic == swapV4 && len(log.Topics) > 1 {
			key, ok = v4[log.Topics[1]]
			ok = ok && key.Address == log.Address
		}
		if !ok {
			key = domain.PoolKey{Address: log.Address}
			if topic == swapV4 {
				if len(log.Topics) < 2 {
					continue
				}
				key.Kind = domain.PoolKindV4
				key.PoolID = log.Topics[1]
			}
			if !b.unknown[key] && len(b.unknown) >= 4096 {
				continue
			}
			b.unknown[key] = true
		}
		if b.buckets[key] == nil {
			b.buckets[key] = map[uint64]uint64{}
		}
		b.buckets[key][ref.Number/span*span]++
		b.last[key] = ref.Number
		if log.TxHash != (domain.Hash{}) {
			if routed[log.TxHash] == nil {
				routed[log.TxHash] = map[domain.PoolKey]bool{}
			}
			routed[log.TxHash][key] = true
		}
	}
	for _, keys := range routed {
		if len(keys) > 1 {
			for key := range keys {
				b.multiSwap[key]++
			}
		}
	}
}
func (b *ActivityBook) Snapshot() map[domain.PoolKey]Activity {
	out := map[domain.PoolKey]Activity{}
	for key, buckets := range b.buckets {
		stats := Activity{ProfitableUses: b.profitable[key], MultiSwapUses: b.multiSwap[key], LastBlock: b.last[key]}
		for _, count := range buckets {
			stats.Swaps += count
		}
		out[key] = stats
	}
	for key, count := range b.profitable {
		row := out[key]
		row.ProfitableUses = count
		out[key] = row
	}
	return out
}
func (b *ActivityBook) Profitable(keys []domain.PoolKey) {
	if b.profitable == nil {
		b.profitable = map[domain.PoolKey]uint64{}
	}
	for _, key := range keys {
		b.profitable[key]++
	}
}
func (b *ActivityBook) Unknown() []domain.PoolKey {
	out := []domain.PoolKey{}
	for key := range b.unknown {
		out = append(out, key)
	}
	slices.SortFunc(out, domain.ComparePoolKeys)
	return out
}

// ActivityFor associates an unclassified Swap emitter with its verified protocol key.
func ActivityFor(stats map[domain.PoolKey]Activity, key domain.PoolKey) Activity {
	if row, ok := stats[key]; ok {
		return row
	}
	if !key.IsV4() {
		key.Kind = 0
	}
	return stats[key]
}
