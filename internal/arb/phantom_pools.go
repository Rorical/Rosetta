package arb

import (
	"log/slog"
	"maps"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A pool whose coarse estimate keeps failing the exact quote fills top-K with
// phantom paths: one MEV-taxed Slipstream pool and its V4 pair can appear in
// 80% of exact-quoted candidates without ever quoting positive. Such a pool
// leaves coarse ranking
// for a while; any positive exact quote through it resets its count. A pool
// that quotes positive one time in ten misses 512 times in a row with
// probability about 1e-24, so busy pools are not mistaken for phantoms. A pool
// with a positive quote in the last phantomRankings rankings counts no misses:
// routes failing through a pool someone deployed to fail cannot push a hub on
// the same routes out of ranking.
const (
	phantomMisses   = 512  // straight non-positive exact quotes through a pool
	phantomRankings = 3000 // rankings it stays out: minutes at OP's frame rate
	phantomTracked  = 8192 // pools with a miss count before the counts restart
)

type phantomPools struct {
	mu       sync.Mutex
	misses   map[domain.PoolKey]int
	positive map[domain.PoolKey]uint64 // the ranking of the pool's last positive quote
	until    map[domain.PoolKey]uint64 // the ranking at which an excluded pool returns
	rankings uint64
	excluded atomic.Pointer[map[domain.PoolKey]struct{}]
}

// InheritPhantoms carries excluded pools and miss counts into a rebuilt
// topology's engine, as RememberCycles carries observed routes.
func (e *Engine) InheritPhantoms(previous *Engine) {
	if previous == nil || previous == e {
		return
	}
	previous.phantoms.mu.Lock()
	misses, positive, until, rankings := maps.Clone(previous.phantoms.misses), maps.Clone(previous.phantoms.positive), maps.Clone(previous.phantoms.until), previous.phantoms.rankings
	previous.phantoms.mu.Unlock()
	e.phantoms.mu.Lock()
	defer e.phantoms.mu.Unlock()
	e.phantoms.misses, e.phantoms.positive, e.phantoms.until, e.phantoms.rankings = misses, positive, until, rankings
	e.phantoms.publish()
}

// observe records one finished exact quote of a route.
func (p *phantomPools) observe(route []domain.PoolKey, positive bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.misses == nil || len(p.misses) >= phantomTracked {
		p.misses = make(map[domain.PoolKey]int)
	}
	if p.positive == nil || len(p.positive) >= phantomTracked {
		p.positive = make(map[domain.PoolKey]uint64)
	}
	changed := false
	for _, key := range route {
		if positive {
			delete(p.misses, key)
			p.positive[key] = p.rankings
			continue
		}
		if last, ok := p.positive[key]; ok && p.rankings-last < phantomRankings {
			continue
		}
		p.misses[key]++
		if _, out := p.until[key]; !out && p.misses[key] >= phantomMisses {
			if p.until == nil {
				p.until = make(map[domain.PoolKey]uint64)
			}
			p.until[key] = p.rankings + phantomRankings
			p.misses[key] = phantomMisses / 2
			changed = true
			slog.Info("pool left coarse ranking", "pool", key.Address.Hex(), "kind", key.Kind, "pool_id", key.PoolID.Hex(), "misses", phantomMisses, "rankings", phantomRankings)
		}
	}
	if changed {
		p.publish()
	}
}

// beginRanking counts one coarse ranking and returns pools whose time is up.
func (p *phantomPools) beginRanking() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rankings++
	changed := false
	for key, until := range p.until {
		if p.rankings >= until {
			delete(p.until, key)
			changed = true
		}
	}
	if changed {
		p.publish()
	}
}

// excludes reports whether a route crosses a pool left out of ranking.
func (p *phantomPools) excludes(route []domain.PoolKey) bool {
	excluded := p.excluded.Load()
	if excluded == nil {
		return false
	}
	for _, key := range route {
		if _, out := (*excluded)[key]; out {
			return true
		}
	}
	return false
}

// publish replaces the excluded set read lock-free by ranking. The caller holds mu.
func (p *phantomPools) publish() {
	if len(p.until) == 0 {
		p.excluded.Store(nil)
		return
	}
	excluded := make(map[domain.PoolKey]struct{}, len(p.until))
	for key := range p.until {
		excluded[key] = struct{}{}
	}
	p.excluded.Store(&excluded)
}
