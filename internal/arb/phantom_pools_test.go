package arb

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A MEV-taxed Slipstream pool can appear in tens of thousands of exact-quoted
// candidates and never quote positive. After phantomMisses straight misses a
// pool leaves coarse ranking until phantomRankings rankings pass; a positive
// quote through it resets the count.
func TestPhantomPoolsLeaveRankingAfterRepeatedMisses(t *testing.T) {
	var p phantomPools
	phantom, hub := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}
	route := []domain.PoolKey{phantom, hub}
	for i := range phantomMisses - 1 {
		p.observe(route, false)
		if i == phantomMisses/2 {
			p.observe([]domain.PoolKey{hub}, true)
		}
	}
	if p.excludes(route) {
		t.Fatal("excluded before the miss threshold")
	}
	p.observe(route, false)
	if !p.excludes([]domain.PoolKey{phantom}) || p.excludes([]domain.PoolKey{hub}) {
		t.Fatal("a pool missing every quote must leave ranking; one with a recent positive must not")
	}
	for range phantomRankings - 1 {
		p.beginRanking()
	}
	if !p.excludes([]domain.PoolKey{phantom}) {
		t.Fatal("returned early")
	}
	p.beginRanking()
	if p.excludes([]domain.PoolKey{phantom}) {
		t.Fatal("never returned")
	}
	// Back in ranking with half its misses: still a phantom, it leaves sooner.
	for range phantomMisses / 2 {
		p.observe([]domain.PoolKey{phantom}, false)
	}
	if !p.excludes([]domain.PoolKey{phantom}) {
		t.Fatal("a returning phantom must leave again after half the misses")
	}
}

// A pool that quoted positive within phantomRankings keeps no misses, so routes
// failing through a new pool cannot push a busy hub out of ranking.
func TestPhantomPoolsSpareRecentlyPositivePools(t *testing.T) {
	var p phantomPools
	hub, attacker := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}
	p.observe([]domain.PoolKey{hub}, true)
	for range phantomMisses * 4 {
		p.observe([]domain.PoolKey{hub, attacker}, false)
	}
	if p.excludes([]domain.PoolKey{hub}) || !p.excludes([]domain.PoolKey{attacker}) {
		t.Fatal("a recently positive hub was excluded, or the failing pool was not")
	}
	for range phantomRankings {
		p.beginRanking()
	}
	for range phantomMisses {
		p.observe([]domain.PoolKey{hub}, false)
	}
	if !p.excludes([]domain.PoolKey{hub}) {
		t.Fatal("a hub with no positive quote for phantomRankings rankings is not spared")
	}
}
