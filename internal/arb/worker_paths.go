package arb

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func (w *Worker) beginPath(ctx context.Context, job SearchJob, keys []domain.PoolKey) (context.Context, func(), bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.unchanged(job, keys) || ctx.Err() != nil {
		return nil, nil, false
	}
	pathCtx, cancel := context.WithCancel(ctx)
	source := searchSlot(job.Version.Source)
	path := &activeSearchPath{keys: keys, cancel: cancel}
	if w.paths[source] == nil {
		w.paths[source] = make(map[*activeSearchPath]struct{})
	}
	w.paths[source][path] = struct{}{}
	return pathCtx, func() {
		cancel()
		w.mu.Lock()
		delete(w.paths[source], path)
		w.mu.Unlock()
	}, true
}

func (w *Worker) pathCurrent(job SearchJob, keys []domain.PoolKey) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.unchanged(job, keys)
}

type QuoteStage string

const (
	QuoteInitial   QuoteStage = "initial"
	QuoteOptimized QuoteStage = "optimized"
)

func (w *Worker) emitCandidate(ctx context.Context, job SearchJob, quote Opportunity, first bool, stage QuoteStage, quotedAt time.Time) error {
	// The channel is bounded. Backpressure retains explicit ownership and is
	// interruptible; candidates are never silently dropped for a full queue.
	owned := job
	owned.Observe, owned.Prepare, owned.PrepareExact = nil, nil, nil
	owned.initials = nil
	owned.Release = job.frame.retain()
	event := SearchEvent{Stages: job.Stages, Job: owned, Stage: stage, QuotedAt: quotedAt, Partial: true, First: first, Result: SearchResult{Opportunities: []Opportunity{quote}},
		receipt: &searchReceipt{origin: owned, dependencies: slices.Clone(quote.Cycle.Pools)}}
	event = w.trackDelivery(event)
	select {
	case w.results <- event:
		return nil
	case <-ctx.Done():
		releaseSearch(event.Job)
		return ctx.Err()
	}
}

func (w *Worker) optimizePaths(ctx context.Context, job *SearchJob, engine *Engine, selection RankedSelection, cache *exactPathCache) (SearchResult, error) {
	result := SearchResult{Considered: selection.considered, Issues: slices.Clone(selection.issues)}
	if len(selection.candidates) == 0 {
		return result, ctx.Err()
	}
	snapshotStarted := time.Now()
	quotes := quoteViews.Get().(*quoteView)
	quotes.source = job.View
	defer quotes.release()
	// Resolve reuse before copying ticks. Only cache misses need snapshot data;
	// all reads run on the lane owner, then the maps become immutable.
	tasks := make([]pathTask, len(selection.candidates))
	for index, candidate := range interleaveAssets(selection.candidates) {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		cached, reused := cache.find(candidate, w.pathCurrent)
		tasks[index] = pathTask{candidate: candidate, cached: cached, reused: reused}
		if reused {
			continue
		}
		for _, key := range candidate.cycle.Pools {
			if state, ok := quotes.Pool(key); ok && needsDeep(state.Key.Kind) {
				quotes.Deep(key)
			}
		}
	}
	quotes.source = nil
	job.Stages.Snapshot += time.Since(snapshotStarted)
	job.exactReadyAt = time.Now()
	workers := min(w.cpus.capacity, len(selection.candidates))
	outcomes := make([]pathOutcome, len(selection.candidates))
	groupCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var next atomic.Uint64
	var localFirst atomic.Bool
	first := &localFirst
	var group sync.WaitGroup
	run := func() {
		for groupCtx.Err() == nil {
			index := int(next.Add(1) - 1)
			if index >= len(selection.candidates) {
				return
			}
			waitStarted := time.Now()
			lease, err := w.cpus.acquireClass(groupCtx, 1, routeClass(len(tasks[index].candidate.cycle.Pools)))
			if err != nil {
				return
			}
			pathJob := *job
			pathJob.Stages.PathCPU = time.Since(waitStarted)
			outcomes[index] = w.optimizePath(groupCtx, pathJob, engine, tasks[index], quotes, cache, first, &lease)
			if outcomes[index].err != nil {
				cancel()
				return
			}
		}
	}
	if w.opts.StreamInitial {
		// A bounded one-amount sweep gives every selected path a chance to publish
		// before any worker starts the much longer amount refinement. Negative or
		// failed probes still receive the complete original sizing search.
		var probeNext atomic.Uint64
		var probes sync.WaitGroup
		probe := func() {
			for groupCtx.Err() == nil {
				index := int(probeNext.Add(1) - 1)
				if index >= len(tasks) {
					return
				}
				if !tasks[index].initialPublished {
					tasks[index].initialPublished = w.probePath(groupCtx, *job, engine, tasks[index], quotes, first)
				}
			}
		}
		if workers == 1 {
			probe()
		} else {
			for range workers {
				probes.Go(probe)
			}
			probes.Wait()
		}
	}
	if workers == 1 {
		run()
	} else {
		for range workers {
			group.Go(run)
		}
		group.Wait() // Readers and shared snapshot maps outlive every exact task.
	}
	var failure error
	for index, path := range outcomes {
		if tasks[index].initialPublished {
			result.Published++
		}
		result.Selected += path.selected
		if path.canceled {
			result.ExactCanceled++
		}
		if path.reused {
			result.ExactReused++
		}
		for _, issue := range path.issues {
			result.Issues = appendIssue(result.Issues, issue)
		}
		result.Limits.add(path.limits)
		if path.err != nil && (failure == nil || failure == context.Canceled) {
			failure = path.err
		}
		result.Published += path.published
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	return result, failure
}

type pathTask struct {
	candidate        rankedCycle
	cached           exactPathResult
	reused           bool
	initialPublished bool
}

type pathOutcome struct {
	selected, published int
	reused, canceled    bool
	issues              []QuoteIssue
	limits              QuoteLimits
	err                 error
}

func (w *Worker) optimizePath(ctx context.Context, job SearchJob, engine *Engine, task pathTask, quotes *quoteView, cache *exactPathCache, first *atomic.Bool, lease *searchCPULease) (out pathOutcome) {
	defer lease.release()
	pathStarted := time.Now()
	job.Stages.PathQueue = max(0, pathStarted.Sub(job.exactReadyAt)-job.Stages.PathCPU)
	initialCPUWait := job.Stages.PathCPU
	candidate := task.candidate
	pathCtx, done, current := w.beginPath(ctx, job, candidate.cycle.Pools)
	if !current {
		out.canceled = true
		return out
	}
	defer done()
	out.selected = 1
	cached, reused := task.cached, task.reused
	out.reused = reused
	var opportunity Opportunity
	var ok bool
	if reused {
		opportunity, ok = copyOpportunity(cached.quote), cached.ok
	} else {
		var initial func(Opportunity) error
		if w.opts.StreamInitial && !task.initialPublished {
			initial = func(quote Opportunity) error {
				if !w.opts.publishable(quote) {
					return nil
				}
				quotedAt := time.Now()
				job.Stages.Math = quotedAt.Sub(pathStarted)
				quote = copyOpportunity(quote) // Sizing reuses its integer/leg buffers.
				engine.priceGas(&quote)
				lease.release()
				publicationStarted := time.Now()
				if err := w.emitCandidate(pathCtx, job, quote, first.CompareAndSwap(false, true), QuoteInitial, quotedAt); err != nil {
					return err
				}
				job.Stages.PriorPublication += time.Since(publicationStarted)
				out.published++
				var err error
				resumeStarted := time.Now()
				*lease, err = w.cpus.acquireClass(pathCtx, 1, routeClass(len(candidate.cycle.Pools)))
				job.Stages.PathCPU += time.Since(resumeStarted)
				return err
			}
		}
		opportunity, out.issues, ok, out.err = engine.optimizeCycleObserved(pathCtx, quotes, candidate, initial, &out.limits)
	}
	if pathCtx.Err() != nil {
		out.canceled = true
		out.err = ctx.Err()
		out.issues = nil
		return out
	}
	if out.err != nil {
		return out
	}
	if !reused && len(out.issues) == 0 {
		cache.store(candidate, job, opportunity, ok)
	}
	if !ok || !w.opts.publishable(opportunity) || !w.pathCurrent(job, candidate.cycle.Pools) {
		return out
	}
	engine.priceGas(&opportunity)
	quotedAt := time.Now()
	job.Stages.Math = max(0, quotedAt.Sub(pathStarted)-job.Stages.PriorPublication-(job.Stages.PathCPU-initialCPUWait))
	lease.release() // Publication backpressure must not reserve compute capacity.
	if err := w.emitCandidate(pathCtx, job, opportunity, first.CompareAndSwap(false, true), QuoteOptimized, quotedAt); err != nil {
		out.canceled = true
		out.err = ctx.Err()
		return out
	}
	out.published++
	return out
}
