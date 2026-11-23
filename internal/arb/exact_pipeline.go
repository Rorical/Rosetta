package arb

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// One lane's admitted representatives plus final K bound the queue. The owner
// prepares snapshots; workers only see immutable per-path maps. A task yields
// after eight amount attempts, retaining its workspace and refinement bracket.
// New probes precede continuations, with a bounded burst to prevent starvation.
// With an ExactK budget, candidates past it still run their one-amount probe:
// that initial quote is what streaming already publishes before sizing ends,
// so consumers see the same candidates; only the refinement is skipped.
type exactPipeline struct {
	worker *Worker
	ctx    context.Context
	cache  *exactPathCache
	first  *atomic.Bool
	memo   *legQuoteMemo
	mu     sync.Mutex
	ready  *sync.Cond
	// idle is signaled when the last outstanding task finishes.
	idle *sync.Cond
	// probes and pending hold routes longer than shortRouteHops; next takes
	// the short queues first among probes and among continuations.
	probes, pending           exactQueue
	shortProbes, shortPending exactQueue
	outstanding               atomic.Int64
	seen                      routeSet
	limit                     int
	budgets                   map[domain.Address]*exactBudget
	burst                     int
	resumeBurst               int
	lastGroup                 exactGroup
	closed                    bool
	group                     sync.WaitGroup
	result                    SearchResult
	err                       error
}

func newExactPipeline(ctx context.Context, w *Worker, cache *exactPathCache, first *atomic.Bool, limit int) *exactPipeline {
	p := &exactPipeline{worker: w, ctx: ctx, cache: cache, first: first, memo: newLegQuoteMemo(), limit: limit, budgets: make(map[domain.Address]*exactBudget)}
	p.ready = sync.NewCond(&p.mu)
	p.idle = sync.NewCond(&p.mu)
	for range w.cpus.capacity {
		p.group.Go(p.run)
	}
	return p
}

// enqueue runs only on the lane owner. The shared quote cache is extended here,
// then each task receives its own immutable maps before workers are notified.
func (p *exactPipeline) enqueue(job *SearchJob, engine *Engine, candidates []rankedCycle) {
	var tasks []*exactTask
	owner := job.initials
	if owner.quotes == nil {
		owner.quotes = quoteViews.Get().(*quoteView)
	}
	quotes := owner.quotes
	started := time.Now()
	quotes.source = job.View
	for _, candidate := range interleaveAssets(candidates) {
		if p.seen.contains(candidate, sameExactInput) {
			continue
		}
		p.seen.add(candidate)
		cached, reused := p.cache.find(candidate, p.worker.pathCurrent)
		// A cached result republishes without sizing, so it spends no budget.
		probeOnly := !reused && !grantSizing(p.budgets, candidate, p.limit)
		var frozen *quoteView
		if !reused {
			frozen = quoteViews.Get().(*quoteView)
			frozen.memo = p.memo
			for _, key := range candidate.cycle.Pools {
				state, ok := quotes.Pool(key)
				frozen.shallow[key] = shallowQuoteState{state, ok}
				if ok && needsDeep(state.Key.Kind) {
					deep, found := quotes.Deep(key)
					frozen.deep[key] = deepQuoteState{deep, found}
				}
			}
		}
		tasks = append(tasks, &exactTask{engine: engine, candidate: candidate, quotes: frozen, cached: cached, reused: reused, probeOnly: probeOnly})
	}
	quotes.source = nil
	job.Stages.Snapshot += time.Since(started)
	now := time.Now()
	for _, task := range tasks {
		task.job = *job
		task.job.initials = nil
		task.job.Prepare = nil
		task.job.PrepareExact = nil
		task.job.exactReadyAt = now
	}
	for _, task := range tasks {
		p.expect(task)
	}
	p.mu.Lock()
	for _, task := range tasks {
		probes, _ := p.queues(task)
		probes.push(task)
	}
	p.outstanding.Add(int64(len(tasks)))
	p.ready.Broadcast()
	p.mu.Unlock()
}

// next pops probes before continuations, with a bounded burst so continuations
// still advance; short routes go first within each. A deep route's one-amount
// probe still precedes short sizing, so its discovery is not delayed.
func (p *exactPipeline) next() *exactTask {
	p.mu.Lock()
	defer p.mu.Unlock()
	for p.probes.count+p.pending.count+p.shortProbes.count+p.shortPending.count == 0 && !p.closed {
		p.ready.Wait()
	}
	if p.shortProbes.count+p.probes.count > 0 && (p.burst < 4 || p.shortPending.count+p.pending.count == 0) {
		p.burst++
		if p.shortProbes.count > 0 {
			return p.shortProbes.pop()
		}
		return p.probes.pop()
	}
	pending := &p.shortPending
	if pending.count == 0 {
		pending = &p.pending
	}
	if pending.count == 0 {
		return nil
	}
	p.burst = 0
	var task *exactTask
	if p.resumeBurst == 4 {
		task = pending.pop()
		p.resumeBurst = 0
	} else {
		task = pending.popRanked(p.lastGroup)
		p.resumeBurst++
	}
	p.lastGroup = taskGroup(task)
	return task
}

// expect counts a short route's task about to be queued as CPU demand that
// long routes yield to.
func (p *exactPipeline) expect(task *exactTask) {
	if routeClass(len(task.candidate.cycle.Pools)) == cpuUrgent {
		task.expected = true
		p.worker.cpus.expect(1)
	}
}

// class is the CPU class of task's next quantum: a short route is urgent
// while its job is the newest of its source and backlog once a newer started.
func (p *exactPipeline) class(task *exactTask) cpuClass {
	class := routeClass(len(task.candidate.cycle.Pools))
	if class == cpuUrgent && p.worker.runs[searchSlot(task.job.Version.Source)].newest.Load() != task.job.ticket {
		return cpuBacklog
	}
	return class
}

// queues are the probe and continuation queues of task's route length.
func (p *exactPipeline) queues(task *exactTask) (probes, pending *exactQueue) {
	if routeClass(len(task.candidate.cycle.Pools)) == cpuUrgent {
		return &p.shortProbes, &p.shortPending
	}
	return &p.probes, &p.pending
}

func (p *exactPipeline) run() {
	for {
		task := p.next()
		if task == nil {
			return
		}
		done, err := task.step(p)
		lease := task.lease
		task.lease = searchCPULease{}
		if err != nil && task.ctx != nil && task.ctx.Err() != nil {
			// A changed pool retires only this path, not the other queued work.
			task.canceled, done, err = true, true, p.ctx.Err()
		}
		if !done && err == nil {
			// Queued again before its core is returned, so the core is kept
			// for short routes (expect). Another worker may own task from here.
			p.expect(task)
			p.mu.Lock()
			_, pending := p.queues(task)
			pending.push(task)
			p.ready.Signal()
			p.mu.Unlock()
			lease.release()
			continue
		}
		lease.release()
		outcome := task.finish()
		p.mu.Lock()
		if p.outstanding.Add(-1) == 0 {
			p.idle.Broadcast()
		}
		p.result = mergeSearchResults(p.result, outcome)
		if err != nil && p.err == nil {
			p.err = err
		}
		p.mu.Unlock()
	}
}

// waitIdle blocks the lane owner until every queued task has finished. A task
// ends on its own: sizing is bounded, and a blocked publication follows the
// job's context.
func (p *exactPipeline) waitIdle() {
	p.mu.Lock()
	for p.outstanding.Load() > 0 {
		p.idle.Wait()
	}
	p.mu.Unlock()
}

// close joins actual readers before the lane releases its frame. A canceled
// owner still drains queued tasks so their workspaces and snapshots are freed.
func (p *exactPipeline) close() (SearchResult, error) {
	p.mu.Lock()
	p.closed = true
	p.ready.Broadcast()
	p.mu.Unlock()
	p.group.Wait()
	return p.result, p.err
}
