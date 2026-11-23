package poolrisk

import (
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
)

const (
	quarantineReverts = 3
	quarantineRoutes  = 2
	trackedPools      = 4096
)

// Tracker attributes executed outcomes to pools: OP's execution paper and the
// Nitro reactive preflight, both run at the quote state. The executor does not
// report which leg failed, so a revert of a positively quoted route implicates
// only its pools that have never succeeded in this process. A pool that has
// executed successfully is never blamed: otherwise anyone could pair a healthy
// pool with always-reverting decoys and get it quarantined. An unproven pool
// is quarantined after quarantineReverts reverts across at least
// quarantineRoutes distinct routes.
type Tracker struct {
	mu     sync.Mutex
	stats  map[domain.PoolKey]*poolStats
	proven map[domain.PoolKey]bool
}

type poolStats struct {
	reverts int
	routes  map[domain.Hash]bool
}

// Observe records one execution at its discovery state and returns evidence
// for each pool that has just crossed the quarantine threshold.
func (t *Tracker) Observe(route domain.Hash, pools []domain.PoolKey, reverted bool, block domain.BlockRef) []Evidence {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stats == nil {
		t.stats = make(map[domain.PoolKey]*poolStats)
		t.proven = make(map[domain.PoolKey]bool)
	}
	var evidence []Evidence
	seen := make(map[domain.PoolKey]bool, len(pools))
	for _, key := range pools {
		if seen[key] {
			continue
		}
		seen[key] = true
		if !reverted {
			delete(t.stats, key)
			if len(t.proven) >= trackedPools {
				clear(t.proven)
			}
			t.proven[key] = true
			continue
		}
		if t.proven[key] {
			continue
		}
		stats := t.stats[key]
		if stats == nil {
			if len(t.stats) >= trackedPools {
				clear(t.stats)
			}
			stats = &poolStats{routes: make(map[domain.Hash]bool)}
			t.stats[key] = stats
		}
		stats.reverts++
		stats.routes[route] = true
		if stats.reverts >= quarantineReverts && len(stats.routes) >= quarantineRoutes {
			evidence = append(evidence, Evidence{Pool: key, Reverts: stats.reverts, Routes: len(stats.routes), Block: block, Source: "execution_paper"})
			delete(t.stats, key)
		}
	}
	return evidence
}
