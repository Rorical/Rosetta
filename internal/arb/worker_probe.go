package arb

import (
	"context"
	"sync/atomic"
	"time"
)

// probePath bounds first-pass work to one model amount per uncached route.
// It never caches a rejection or replaces full sizing with a fixed notional.
func (w *Worker) probePath(ctx context.Context, job SearchJob, engine *Engine, task pathTask, quotes *quoteView, first *atomic.Bool) bool {
	if task.reused || task.candidate.input == nil {
		return false
	}
	waitStarted := time.Now()
	lease, err := w.cpus.acquireClass(ctx, 1, routeClass(len(task.candidate.cycle.Pools)))
	if err != nil {
		return false
	}
	defer lease.release()
	job.Stages.PathCPU = time.Since(waitStarted)
	started := time.Now()
	job.Stages.PathQueue = max(0, started.Sub(job.exactReadyAt)-job.Stages.PathCPU)
	pathCtx, done, current := w.beginPath(ctx, job, task.candidate.cycle.Pools)
	if !current {
		return false
	}
	defer done()
	quote, err := engine.QuoteCycle(pathCtx, quotes, task.candidate.cycle, task.candidate.input)
	if err != nil || !w.opts.publishable(quote) || !w.pathCurrent(job, task.candidate.cycle.Pools) {
		return false
	}
	quotedAt := time.Now()
	job.Stages.Math = quotedAt.Sub(started)
	engine.priceGas(&quote)
	lease.release()
	return w.emitCandidate(pathCtx, job, quote, first.CompareAndSwap(false, true), QuoteInitial, quotedAt) == nil
}
