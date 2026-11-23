package arb

import (
	"context"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// searchLane is what the jobs of one source share. A rank cache has one
// owner: short serves the job in the rank slot and long the job in the long
// slot. Each holds per-route revisions, about 26 bytes a route.
type searchLane struct {
	exact       exactPathCache
	short, long rankCache
}

func (w *Worker) take(ctx context.Context, source domain.StateSource) (SearchJob, context.Context, *searchRun, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	job, ok := w.pending[source]
	fromRetry := !ok
	if !ok {
		job, ok = w.retries[source]
	}
	if !ok || ctx.Err() != nil {
		return SearchJob{}, nil, nil, false
	}
	if fromRetry {
		delete(w.retries, source)
	} else {
		delete(w.pending, source)
	}
	jobCtx, cancel := context.WithCancel(ctx)
	run := &searchRun{ticket: job.ticket, cancel: cancel, left: make(chan struct{})}
	if job.Incremental {
		job.batch = &searchBatch{}
		// A dependency retry keeps its original basis and pools.
		if fromRetry {
			job.shortTriggers, job.longTriggers = job.Dirty, job.Dirty
		} else {
			// A job that reads its ranking dependencies over RPC (Prepare)
			// ranks in one pass: a second phase would read them again.
			job.phased = w.opts.StreamInitial && job.Prepare == nil
			w.owe(source).claim(&job, job.phased)
		}
	}
	job.engine = w.engine
	lane := w.runs[source]
	lane.rank, lane.alive = run, append(lane.alive, run)
	lane.newest.Store(job.ticket)
	return job, jobCtx, run, true
}

func (w *Worker) runLane(ctx context.Context, source domain.StateSource) {
	lane := new(searchLane)
	room := make(chan struct{}, laneRunLimit)
	var jobs sync.WaitGroup
	// Cancellation never releases a reader that is still executing.
	defer jobs.Wait()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wakes[source]:
		}
		for {
			select {
			case <-ctx.Done():
				return
			case room <- struct{}{}:
			}
			job, jobCtx, run, ok := w.take(ctx, source)
			if !ok {
				<-room
				break
			}
			jobs.Go(func() {
				completed := w.search(jobCtx, job, run, lane)
				w.retire(source, run, completed)
				<-room
			})
			<-run.left
		}
	}
}

// search evaluates one job and delivers its final event. It reports whether
// the job ended without cancellation or error.
func (w *Worker) search(ctx context.Context, job SearchJob, run *searchRun, lane *searchLane) bool {
	defer run.cancel()
	started := time.Now()
	job.Stages = SearchStages{}
	job.Stages.Queue = started.Sub(job.queuedAt)
	if !job.ReceivedAt.IsZero() {
		job.Stages.Intake = max(0, job.queuedAt.Sub(job.ReceivedAt))
	}
	var result SearchResult
	var err error
	if job.phased {
		result, err = w.evaluatePhased(ctx, &job, run, lane, started)
	} else {
		result, err = w.evaluate(ctx, &job, run, &lane.short, &lane.exact)
	}
	job.initials = nil
	canceled := ctx.Err() != nil || err == context.Canceled
	completed := !canceled && err == nil
	if completed && job.Incremental {
		if !job.phased {
			w.settle(&job, false)
		}
		if job.long != LongDeferred && job.long != LongPreempted {
			w.settle(&job, true)
		}
	}
	if job.Observe != nil {
		outcome := SearchCompleted
		var boundary SearchBoundary
		if canceled {
			outcome = SearchCanceled
			w.mu.Lock()
			boundary = run.boundary
			w.mu.Unlock()
		}
		job.Observe(SearchObservation{Boundary: boundary, Stages: job.Stages, QueueWait: started.Sub(job.queuedAt), Evaluation: time.Since(started), Outcome: outcome, Failed: err != nil, PathCanceled: uint64(result.ExactCanceled), PathReused: uint64(result.ExactReused),
			Overtaken: job.overtaken, Overlapped: job.overlapped, Frames: job.frames, Long: job.long, Resumed: job.resumed})
		job.Observe = nil
	}
	event := w.finalEvent(job, result, err)
	if !canceled {
		select {
		case w.results <- event:
			return completed
		case <-ctx.Done():
		}
	}
	releaseSearch(event.Job)
	return false
}
