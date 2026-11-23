package arb

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// closeExact joins the job's exact pipeline into its result. No reader is
// released while a quote still runs.
func closeExact(job *SearchJob, result *SearchResult, err *error) {
	p := job.initials
	if p == nil {
		return
	}
	if p.pipeline != nil {
		exactResult, exactErr := p.pipeline.close()
		*result = mergeSearchResults(*result, exactResult)
		if *err == nil {
			*err = exactErr
		}
	}
	for _, issue := range p.issues {
		result.Issues = appendIssue(result.Issues, issue)
	}
	if p.quotes != nil {
		p.quotes.release()
	}
}

// dependencies are the pools on any route through keys, with extra.
func dependencies(engine *Engine, keys, extra []domain.PoolKey) []domain.PoolKey {
	pools := append(graph.Dependencies(engine.table, keys), extra...)
	slices.SortFunc(pools, domain.ComparePoolKeys)
	return slices.Compact(pools)
}

func explorationStats(explored graph.Exploration) ExplorationStats {
	return ExplorationStats{Steps: explored.Steps, Discovered: explored.Added, Cached: len(explored.Cycles), CatalogPaths: explored.CatalogPaths, Truncated: explored.BudgetExhausted || explored.CacheTruncated,
		BudgetExhausted: explored.BudgetExhausted, CacheFull: explored.CacheTruncated}
}

// evaluate ranks every route through job.Dirty in one pass. It serves jobs
// that are not phased: unstreamed ones and dependency retries.
func (w *Worker) evaluate(ctx context.Context, job *SearchJob, run *searchRun, cache *rankCache, exact *exactPathCache) (result SearchResult, err error) {
	rankCtx, cancelCoarse := context.WithCancel(ctx)
	defer cancelCoarse()
	if job.Incremental {
		w.beginCoarse(searchSlot(job.Version.Source), run, job.Dirty, cancelCoarse)
		defer w.endCoarse(job, run)
	}
	defer closeExact(job, &result, &err)
	phase := time.Now()
	engine := job.engine
	job.engine = nil
	if engine.compiled != nil {
		cache.bind(engine.compiled, job.Version.Epoch)
	}
	explored, err := cache.explore(rankCtx, engine, job.Dirty)
	job.Stages.Catalog = time.Since(phase)
	if err != nil {
		return SearchResult{}, err
	}
	preparationKeys := job.Dirty
	if len(explored.Cycles) > 0 {
		preparationKeys = cache.cyclePools(job.Dirty, explored.Cycles)
	}
	if job.Incremental {
		job.dependencies = dependencies(engine, job.Dirty, preparationKeys)
	}
	job.Stages.Catalog = time.Since(phase)
	phase = time.Now()
	var issues []QuoteIssue
	if job.Prepare != nil {
		view, preparedIssues, err := job.Prepare(rankCtx, job.View, preparationKeys)
		job.Stages.Prepare = time.Since(phase)
		job.Prepare = nil // Release owns any readers retained by the prepared view
		if err != nil {
			return SearchResult{}, err
		}
		if view == nil {
			return SearchResult{}, fmt.Errorf("%w: prepared search view is nil", ErrInvalidConfig)
		}
		job.View, issues = view, preparedIssues
	}
	job.Stages.Prepare = time.Since(phase)
	if job.Incremental && w.opts.StreamInitial {
		job.initials = &progressQuotes{}
		job.initials.pipeline = newExactPipeline(ctx, w, exact, &job.initials.first, w.exactLimit(engine))
	}
	selection, err := w.rankSelection(rankCtx, job, engine, explored.Cycles, cache)
	if err != nil {
		return SearchResult{}, err
	}
	if job.initials == nil || job.initials.pipeline == nil {
		// The streamed pipeline applies ExactK itself and keeps the probes.
		selection = selection.limitPerAsset(w.exactLimit(engine))
	}

	preparedIssues, err := prepareExact(rankCtx, job, selection.Pools())
	job.PrepareExact = nil
	if err != nil {
		return SearchResult{}, err
	}
	issues = append(issues, preparedIssues...)
	if job.Incremental {
		w.endCoarse(job, run)
	}
	switch {
	case !job.Incremental:
		result, err = engine.Optimize(ctx, job.View, selection)
	case job.initials != nil && job.initials.pipeline != nil:
		job.initials.pipeline.enqueue(job, engine, selection.candidates)
		result = SearchResult{Considered: selection.considered, Issues: selection.issues}
	default:
		result, err = w.optimizePaths(ctx, job, engine, selection, exact)
	}
	result.Exploration = explorationStats(explored)
	for _, issue := range issues {
		result.Issues = appendIssue(result.Issues, issue)
	}
	return result, err
}
