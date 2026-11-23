package arb

import (
	"context"
	"time"
)

type exactTask struct {
	engine            *Engine
	candidate         rankedCycle
	quotes            *quoteView
	cached            exactPathResult
	reused, probeOnly bool
	job               SearchJob
	ctx               context.Context
	release           func()
	search            *amountSearch
	probeDone         bool
	published         int
	canceled          bool
	// expected: the task is queued and counted in searchCPUs.expect.
	expected                       bool
	lease                          searchCPULease
	math, cpu, publication         time.Duration
	quantumStarted                 time.Time
	quantumPublication, quantumCPU time.Duration
}

func (t *exactTask) publish(p *exactPipeline, quote Opportunity, stage QuoteStage) error {
	if !p.worker.opts.publishable(quote) {
		return nil
	}
	now := time.Now()
	job := t.job
	job.Stages.Math = t.math + max(0, now.Sub(t.quantumStarted)-t.quantumPublication-t.quantumCPU)
	job.Stages.PathCPU = t.cpu
	job.Stages.PriorPublication = t.publication
	job.Stages.PathQueue = max(0, now.Sub(job.exactReadyAt)-job.Stages.Math-t.cpu-t.publication)
	quote = copyOpportunity(quote)
	t.engine.priceGas(&quote)
	t.lease.release()
	started := time.Now()
	err := p.worker.emitCandidate(t.ctx, job, quote, p.first.CompareAndSwap(false, true), stage, now)
	elapsed := time.Since(started)
	t.publication += elapsed
	t.quantumPublication += elapsed
	if err != nil {
		return err
	}
	t.published++
	if stage == QuoteOptimized {
		return nil
	}
	started = time.Now()
	t.lease, err = p.worker.cpus.acquireClass(t.ctx, 1, p.class(t))
	elapsed = time.Since(started)
	t.cpu += elapsed
	t.quantumCPU += elapsed
	return err
}

// dequeued ends the task's CPU expectation: it has its core or gave up.
func (t *exactTask) dequeued(p *exactPipeline) {
	if t.expected {
		t.expected = false
		p.worker.cpus.met(1)
	}
}

// step runs one quantum. The caller releases t.lease afterwards.
func (t *exactTask) step(p *exactPipeline) (bool, error) {
	if t.ctx == nil {
		var current bool
		t.ctx, t.release, current = p.worker.beginPath(p.ctx, t.job, t.candidate.cycle.Pools)
		if !current {
			t.dequeued(p)
			t.canceled = true
			return true, nil
		}
	}
	if t.ctx.Err() != nil {
		t.dequeued(p)
		t.canceled = true
		return true, nil
	}
	started := time.Now()
	var err error
	t.lease, err = p.worker.cpus.acquireClass(t.ctx, 1, p.class(t))
	t.dequeued(p)
	t.cpu += time.Since(started)
	if err != nil {
		t.canceled = true
		return true, nil
	}
	t.quantumStarted = time.Now()
	t.quantumPublication = 0
	t.quantumCPU = 0
	defer func() { t.math += max(0, time.Since(t.quantumStarted)-t.quantumPublication-t.quantumCPU) }()
	if t.reused {
		if t.cached.ok && t.cached.quote.Net != nil && t.cached.quote.Net.Sign() > 0 && p.worker.pathCurrent(t.job, t.candidate.cycle.Pools) {
			err = t.publish(p, t.cached.quote, QuoteOptimized)
		}
		return true, err
	}
	if t.search == nil {
		t.search = newAmountSearch(t.engine, t.quotes, t.candidate, func(q Opportunity) error { return t.publish(p, q, QuoteInitial) })
	}
	if !t.probeDone {
		t.probeDone = true
		// The probe becomes an ordinary cached sizing sample, including negative
		// quotes and dependency errors. It does not discard full amount search.
		if t.candidate.input != nil {
			_, err = t.search.quoteAt(t.ctx, t.candidate.input)
		}
		if t.probeOnly && err == nil && t.ctx.Err() == nil {
			t.engine.phantoms.observe(t.candidate.cycle.Pools, t.search.quoted && t.search.best.Net.Sign() > 0)
		}
		return t.probeOnly, err
	}
	done, err := t.search.step(t.ctx, 8)
	if t.ctx.Err() != nil {
		t.canceled = true
		return true, nil
	}
	if err != nil || !done {
		return done, err
	}
	t.engine.phantoms.observe(t.candidate.cycle.Pools, t.search.quoted && t.search.best.Net.Sign() > 0)
	if len(t.search.issues) == 0 {
		p.cache.store(t.candidate, t.job, t.search.best, t.search.quoted)
	}
	if t.search.quoted && t.search.best.Net.Sign() > 0 && p.worker.pathCurrent(t.job, t.candidate.cycle.Pools) {
		err = t.publish(p, t.search.best, QuoteOptimized)
	}
	return true, err
}

func (t *exactTask) finish() SearchResult {
	result := SearchResult{Selected: 1, Published: t.published}
	if t.canceled || (t.ctx != nil && t.ctx.Err() != nil) {
		result.ExactCanceled = 1
	}
	if t.reused {
		result.ExactReused = 1
	}
	if t.probeOnly {
		result.ProbeOnly = 1
	}
	if t.search != nil {
		if result.ExactCanceled == 0 {
			result.Issues = t.search.issues
			result.Limits = t.search.limits
		}
		t.search.close()
	}
	if t.quotes != nil {
		t.quotes.release()
	}
	if t.release != nil {
		t.release()
	}
	return result
}
