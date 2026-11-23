package arb

import (
	"context"
	"time"

	"github.com/Rorical/Rosetta/internal/graph"
)

// rankSelection ranks every route through job.Dirty in one pass, for jobs that
// are not phased.
func (w *Worker) rankSelection(ctx context.Context, job *SearchJob, engine *Engine, explored []graph.Cycle, cache *rankCache) (RankedSelection, error) {
	started := time.Now()
	lease, err := w.cpus.acquire(ctx, w.rankCapacity(job))
	job.Stages.RankCPU = time.Since(started)
	if err != nil {
		return RankedSelection{}, err
	}
	defer func() { lease.release() }()
	started = time.Now()
	var selection, extra RankedSelection
	streamed := job.Incremental && w.opts.StreamInitial
	if streamed {
		if job.initials == nil {
			job.initials = &progressQuotes{}
		}
		catalog := func() error {
			if len(explored) == 0 {
				return nil
			}
			var err error
			extra, err = w.rankCatalog(ctx, job, engine, explored, cache)
			return err
		}
		selection, err = engine.rankScheduled(ctx, job.View, job.Dirty, w.opts, cache, lease.count, w.progressiveSchedule(ctx, job, engine, cache, &lease, started, rankPass{afterFirst: catalog}))
		if job.Stages.Rank == 0 {
			job.Stages.Rank = time.Since(started)
		}
	} else {
		selection, err = engine.rankWithWorkers(ctx, job.View, job.Dirty, w.opts, cache, lease.count)
		job.Stages.Rank += time.Since(started)
	}

	if err != nil || len(explored) == 0 {
		return selection, err
	}
	if streamed {
		return mergeRankedDiverse(engine.K(), w.opts.Diverse, selection, extra), nil
	}
	waiting := time.Now()
	lease.release()
	lease, err = w.cpus.acquire(ctx, w.rankCapacity(job))
	job.Stages.RankCPU += time.Since(waiting)
	if err != nil {
		return RankedSelection{}, err
	}
	started = time.Now()
	extra, err = engine.rankExplored(ctx, job.View, explored, job.Dirty, w.opts, lease.count)
	job.Stages.Rank += time.Since(started)
	if err != nil {
		return RankedSelection{}, err
	}
	return mergeRankedDiverse(engine.K(), w.opts.Diverse, selection, extra), nil
}

// Leave one compute slot for exact work once discovery has admitted candidates.
// Discovery can borrow the full budget while the exact queue is empty.
func (w *Worker) rankCapacity(job *SearchJob) int {
	capacity := w.cpus.capacity
	if p := job.initials; p != nil && p.pipeline != nil && p.pipeline.outstanding.Load() > 0 {
		return max(1, capacity-1)
	}
	return capacity
}
