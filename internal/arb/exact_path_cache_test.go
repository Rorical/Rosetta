package arb

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func cacheCandidate(index int) rankedCycle {
	key := domain.PoolKey{Kind: domain.PoolKindV2}
	key.Address[18], key.Address[19] = byte(index>>8), byte(index)
	return rankedCycle{cycle: graph.Cycle{Start: testAddress(1), Pools: []domain.PoolKey{key, {Kind: domain.PoolKindV2}}, Len: 2}, bounds: InputBounds{Min: big.NewInt(1), Max: big.NewInt(1000)}, input: big.NewInt(2), net: big.NewInt(1)}
}

func BenchmarkExactPathCache(b *testing.B) {
	var cache exactPathCache
	for index := range 512 {
		cache.store(cacheCandidate(index), SearchJob{}, Opportunity{}, false)
	}
	for _, mode := range []string{"hit", "miss"} {
		b.Run(mode, func(b *testing.B) {
			candidate := cacheCandidate(511)
			if mode == "miss" {
				candidate = cacheCandidate(513)
			}
			b.ReportAllocs()
			for b.Loop() {
				cache.find(candidate, func(SearchJob, []domain.PoolKey) bool { return true })
			}
		})
	}
}

func TestExactCacheReplacesVersionsAndChecksFullIdentity(t *testing.T) {
	var cache exactPathCache
	candidate := cacheCandidate(1)
	for ticket := uint64(1); ticket <= 20; ticket++ {
		cache.store(candidate, SearchJob{ticket: ticket}, Opportunity{}, false)
	}
	if len(cache.entries) != 1 {
		t.Fatal("revisions exhausted cache capacity")
	}
	entry, ok := cache.find(candidate, func(job SearchJob, _ []domain.PoolKey) bool { return job.ticket == 20 })
	if !ok || entry.job.ticket != 20 {
		t.Fatal("cache retained obsolete revision")
	}
	changed := cacheCandidate(1)
	changed.bounds.Max = big.NewInt(999)
	if _, ok := cache.find(changed, func(SearchJob, []domain.PoolKey) bool { return true }); ok {
		t.Fatal("changed funding bounds reused old quote")
	}
	collision := cacheCandidate(2)
	cache.index[cache.fingerprint(collision)] = []int{0}
	if _, ok := cache.find(collision, func(SearchJob, []domain.PoolKey) bool { return true }); ok {
		t.Fatal("hash collision reused a different path")
	}
}

func TestExactCacheEvictionRemovesOldIndex(t *testing.T) {
	var cache exactPathCache
	for index := range 600 {
		cache.store(cacheCandidate(index), SearchJob{}, Opportunity{}, false)
	}
	if len(cache.entries) != 512 || len(cache.index) != 512 {
		t.Fatal("cache exceeded numerical bound")
	}
	for index := range 600 {
		_, ok := cache.find(cacheCandidate(index), func(SearchJob, []domain.PoolKey) bool { return true })
		if ok != (index >= 88) {
			t.Fatal("evicted or live entry has incorrect index", index, ok)
		}
	}
}
