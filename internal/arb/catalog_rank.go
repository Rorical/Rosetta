package arb

import (
	"context"
	"time"

	"github.com/Rorical/Rosetta/internal/graph"
)

// The bounded supplemental catalog runs after the first short main batches,
// before the wide main scan. Its opportunities use the same refinement budget.
func (w *Worker) rankCatalog(ctx context.Context, job *SearchJob, engine *Engine, cycles []graph.Cycle, cache *rankCache) (RankedSelection, error) {
	extra, err := New(exploredCycles(cycles), engine.notional, engine.K(), engine.quoter)
	if err != nil {
		return RankedSelection{}, err
	}
	extra.gasCost, extra.gasToken = engine.gasCost, engine.gasToken
	started := time.Now()
	lease, err := w.cpus.acquire(ctx, w.rankCapacity(job))
	job.Stages.RankCPU += time.Since(started)
	if err != nil {
		return RankedSelection{}, err
	}
	defer lease.release()
	if cache.catalogRank == nil {
		cache.catalogRank = new(rankCache)
	}
	return extra.rankScheduled(ctx, job.View, job.Dirty, w.opts, nil, lease.count, w.progressiveSchedule(ctx, job, extra, cache.catalogRank, &lease, time.Now(), rankPass{}))
}
