package arb

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// evaluatePhased runs a streamed incremental job (searchRun): its short
// routes, then, if the long slot is free, the long routes and catalog paths
// of the pools owed theirs.
func (w *Worker) evaluatePhased(ctx context.Context, job *SearchJob, run *searchRun, lane *searchLane, started time.Time) (result SearchResult, err error) {
	source := searchSlot(job.Version.Source)
	engine := job.engine
	job.engine = nil
	job.initials = &progressQuotes{}
	job.initials.pipeline = newExactPipeline(ctx, w, &lane.exact, &job.initials.first, w.exactLimit(engine))
	defer closeExact(job, &result, &err)
	defer w.endCoarse(job, run)

	buckets := make(map[domain.Address]*rankBucket)
	short, issues, err := w.rankShort(ctx, job, run, engine, &lane.short, buckets)
	result = SearchResult{Considered: short.considered, Issues: appendIssues(short.issues, issues)}
	if err != nil {
		return result, err
	}
	// The short routes are ranked and queued for sizing. The pools are
	// acknowledged before the next job, which may start now, takes its own.
	w.settle(job, false)
	long := w.enterLong(job, run)
	w.leaveRank(source, run)
	// Long routes rank after the job's short quotes are out. Long ranking
	// that begins while they are being sized holds most of the cores and
	// stretches short-route sizing from 2.7 ms to 33 ms into a 39 ms frame.
	job.initials.pipeline.waitIdle()
	job.Stages.Short = time.Since(started)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !long {
		job.long = LongDeferred
		return result, nil
	}
	longStarted := time.Now()
	selection, explored, issues, err := w.rankLong(ctx, job, run, engine, &lane.long, buckets)
	job.Stages.Long = time.Since(longStarted)
	result.Considered += selection.considered
	result.Issues = appendIssues(appendIssues(result.Issues, selection.issues), issues)
	result.Exploration = explorationStats(explored)
	if err != nil && ctx.Err() == nil && errors.Is(err, context.Canceled) {
		// Newer frames changed every pool the long routes were ranked
		// through. The job ends with its short routes delivered and the
		// pools still owed their long routes.
		job.long = LongPreempted
		return result, nil
	}
	if err != nil {
		return result, err
	}
	job.initials.pipeline.enqueue(job, engine, selection.candidates)
	job.long = LongCompleted
	return result, nil
}

func appendIssues(issues, extra []QuoteIssue) []QuoteIssue {
	for _, issue := range extra {
		issues = appendIssue(issues, issue)
	}
	return issues
}

// rankShort ranks every short route through the job's short triggers into
// buckets and queues their selection for sizing. Before the phases, the
// selection was made once, after the last four-hop batch, so the short routes
// it added were sized last, and never when a newer frame preempted the
// rotation.
func (w *Worker) rankShort(ctx context.Context, job *SearchJob, run *searchRun, engine *Engine, cache *rankCache, buckets map[domain.Address]*rankBucket) (RankedSelection, []QuoteIssue, error) {
	rankCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !w.beginCoarse(searchSlot(job.Version.Source), run, job.shortTriggers, cancel) {
		return RankedSelection{}, nil, rankCtx.Err()
	}
	phase := time.Now()
	if engine.compiled != nil {
		cache.bind(engine.compiled, job.Version.Epoch)
	}
	job.dependencies = dependencies(engine, job.shortTriggers, nil)
	job.Stages.Catalog = time.Since(phase)
	waiting := time.Now()
	lease, err := w.cpus.acquireClass(rankCtx, w.rankCapacity(job), cpuUrgent)
	job.Stages.RankCPU = time.Since(waiting)
	if err != nil {
		return RankedSelection{}, nil, err
	}
	defer func() { lease.release() }()
	schedule := w.progressiveSchedule(rankCtx, job, engine, cache, &lease, time.Now(), rankPass{to: shortRouteHops, class: cpuUrgent, buckets: buckets})
	selection, err := engine.rankScheduled(rankCtx, job.View, job.shortTriggers, w.opts, cache, lease.count, schedule)
	if err != nil {
		return selection, nil, err
	}
	issues, err := prepareExact(rankCtx, job, selection.Pools())
	if err != nil {
		return selection, nil, err
	}
	w.endCoarse(job, run)
	job.initials.pipeline.enqueue(job, engine, selection.candidates)
	return selection, issues, nil
}

// rankLong ranks the catalog paths and the long routes through the job's long
// triggers into the buckets its short routes filled, and returns the
// selection over all of them, as one pass over every depth selects.
func (w *Worker) rankLong(ctx context.Context, job *SearchJob, run *searchRun, engine *Engine, cache *rankCache, buckets map[domain.Address]*rankBucket) (RankedSelection, graph.Exploration, []QuoteIssue, error) {
	rankCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !w.beginCoarse(searchSlot(job.Version.Source), run, job.longTriggers, cancel) {
		return RankedSelection{}, graph.Exploration{}, nil, rankCtx.Err()
	}
	phase := time.Now()
	if engine.compiled != nil {
		cache.bind(engine.compiled, job.Version.Epoch)
	}
	explored, err := cache.explore(rankCtx, engine, job.longTriggers)
	if err != nil {
		return RankedSelection{}, explored, nil, err
	}
	preparationKeys := job.longTriggers
	if len(explored.Cycles) > 0 {
		preparationKeys = cache.cyclePools(job.longTriggers, explored.Cycles)
	}
	job.dependencies = dependencies(engine, job.Dirty, slices.Concat(preparationKeys, job.dependencies))
	job.Stages.Catalog += time.Since(phase)
	var extra RankedSelection
	if len(explored.Cycles) > 0 {
		if extra, err = w.rankCatalog(rankCtx, job, engine, explored.Cycles, cache); err != nil {
			return RankedSelection{}, explored, nil, err
		}
	}
	waiting := time.Now()
	lease, err := w.cpus.acquire(rankCtx, w.rankCapacity(job))
	job.Stages.RankCPU += time.Since(waiting)
	if err != nil {
		return RankedSelection{}, explored, nil, err
	}
	defer func() { lease.release() }()
	schedule := w.progressiveSchedule(rankCtx, job, engine, cache, &lease, time.Now(), rankPass{from: shortRouteHops + 1, class: cpuNormal, buckets: buckets})
	selection, err := engine.rankPass(rankCtx, job.View, job.longTriggers, w.opts, cache, lease.count, schedule, false)
	if err != nil {
		return selection, explored, nil, err
	}
	if len(explored.Cycles) > 0 {
		selection = mergeRankedDiverse(engine.K(), w.opts.Diverse, selection, extra)
	}
	issues, err := prepareExact(rankCtx, job, selection.Pools())
	job.PrepareExact = nil
	if err != nil {
		return selection, explored, nil, err
	}
	w.endCoarse(job, run)
	return selection, explored, issues, nil
}
