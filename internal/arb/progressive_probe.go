package arb

import (
	"context"
	"fmt"
	"math/big"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// The lane owns these records and extends its preparation cache. Exact workers
// receive immutable per-task maps and never access this mutable owner state.
type progressQuotes struct {
	pipeline *exactPipeline
	quotes   *quoteView
	first    atomic.Bool
	// attempted holds the routes admitted for exact quotes, and assets their
	// count and best estimated net per funding asset, overall and by length.
	attempted routeSet
	assets    map[domain.Address]*attemptedAsset
	prepared  map[domain.PoolKey]bool
	issues    []QuoteIssue
}

type attemptedAsset struct {
	total int
	best  *big.Int
	// byLength[n] counts the admitted routes of n pools and their best net.
	byLength []attemptedLength
}

type attemptedLength struct {
	count int
	best  *big.Int
}

func prepareExact(ctx context.Context, job *SearchJob, keys []domain.PoolKey) ([]QuoteIssue, error) {
	if job.PrepareExact == nil || len(keys) == 0 {
		return nil, nil
	}
	if p := job.initials; p != nil {
		if p.prepared == nil {
			p.prepared = make(map[domain.PoolKey]bool)
		}
		fresh := make([]domain.PoolKey, 0, len(keys))
		for _, key := range keys {
			if !p.prepared[key] {
				fresh = append(fresh, key)
			}
		}
		keys = fresh
	}
	if len(keys) == 0 {
		return nil, nil
	}
	started := time.Now()
	defer func() { job.Stages.ExactPrepare += time.Since(started) }()
	view, issues, err := job.PrepareExact(ctx, job.View, keys)
	if err != nil {
		return issues, err
	}
	if view == nil {
		return issues, fmt.Errorf("%w: exact dependency view is nil", ErrInvalidConfig)
	}
	job.View = view
	if job.initials != nil {
		for _, key := range keys {
			job.initials.prepared[key] = true
		}
	}
	return issues, nil
}

func (w *Worker) probeRankBatch(ctx context.Context, job *SearchJob, engine *Engine, part RankedSelection, depth, limit int) error {
	p := job.initials
	candidates := p.admit(part, depth, limit)
	if len(candidates) == 0 {
		return ctx.Err()
	}
	selected := RankedSelection{candidates: candidates}
	issues, err := prepareExact(ctx, job, selected.Pools())
	if err != nil {
		return err
	}
	for _, issue := range issues {
		p.issues = appendIssue(p.issues, issue)
	}
	p.pipeline.enqueue(job, engine, candidates)
	return ctx.Err()
}

func (p *progressQuotes) admit(part RankedSelection, depth, limit int) []rankedCycle {
	// Start with four representatives per depth/asset, then admit only a new
	// leader. Reserve K/4 extra probes for later profit leaders, so early small
	// quotes cannot consume every slot before a larger opportunity is ranked.
	candidates := make([]rankedCycle, 0)
	for _, candidate := range interleaveAssets(part.candidates) {
		total, atDepth := 0, 0
		var best, overall *big.Int
		if asset := p.assets[candidate.cycle.Start]; asset != nil {
			total, overall = asset.total, asset.best
			if depth < len(asset.byLength) {
				atDepth, best = asset.byLength[depth].count, asset.byLength[depth].best
			}
		}
		lateLeader := total >= limit && overall != nil && candidate.net.Cmp(overall) > 0 && total < limit+max(1, limit/4)
		if p.attempted.contains(candidate, sameRankedRoute) || (total >= limit && !lateLeader) || (atDepth >= 4 && candidate.net.Cmp(best) <= 0) {
			continue
		}
		p.record(candidate)
		candidates = append(candidates, candidate)
	}
	return candidates
}

func (p *progressQuotes) record(candidate rankedCycle) {
	p.attempted.add(candidate)
	if p.assets == nil {
		p.assets = make(map[domain.Address]*attemptedAsset)
	}
	asset := p.assets[candidate.cycle.Start]
	if asset == nil {
		asset = &attemptedAsset{}
		p.assets[candidate.cycle.Start] = asset
	}
	asset.total++
	if asset.best == nil || candidate.net.Cmp(asset.best) > 0 {
		asset.best = candidate.net
	}
	length := len(candidate.cycle.Pools)
	if length >= len(asset.byLength) {
		asset.byLength = append(asset.byLength, make([]attemptedLength, length+1-len(asset.byLength))...)
	}
	at := &asset.byLength[length]
	at.count++
	if at.best == nil || candidate.net.Cmp(at.best) > 0 {
		at.best = candidate.net
	}
}
