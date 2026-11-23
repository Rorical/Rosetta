package arb

import (
	"fmt"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func (w *Worker) Submit(job SearchJob) error {
	source := searchSlot(job.Version.Source)
	if job.View == nil || job.GraphVersion == 0 || (source != domain.SourceStandard && source != domain.SourceFast) {
		return fmt.Errorf("%w: search job view, source or graph version", ErrInvalidConfig)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("%w: search worker stopped", ErrInvalidConfig)
	}
	if job.GraphVersion != w.graphVersion {
		return fmt.Errorf("%w: search graph version %d, active %d", ErrInvalidConfig, job.GraphVersion, w.graphVersion)
	}
	if job.Incremental {
		return w.submitIncremental(job, source)
	}
	w.next++
	job.ticket = w.next
	owed := w.owe(source)
	for _, key := range job.Dirty {
		owed.add(key, job.ticket)
	}
	for _, key := range job.Recheck {
		owed.add(key, job.ticket)
	}
	job.Dirty = owed.searchable()
	w.latest[source] = job.ticket
	if previous, ok := w.pending[source]; ok {
		supersedeSearch(previous)
	}
	w.runs[source].cancel()
	if len(job.Dirty) == 0 {
		delete(w.pending, source)
		releaseSearch(job)
		return nil
	}
	if job.Observe != nil {
		job.queuedAt = time.Now()
	}
	w.pending[source] = job
	select {
	case w.wakes[source] <- struct{}{}:
	default:
	}
	return nil
}

// Accept consumes a result once, if it is still current. For a job that is
// not incremental it also clears the accumulated dirty pools, unless a newer
// prefix has replaced the result; an incremental job's pools are acknowledged
// by the worker (settle). The caller additionally checks branch and stream
// freshness.
func (w *Worker) Accept(event SearchEvent) bool {
	if event.Err != nil {
		return false
	}
	source := searchSlot(event.Job.Version.Source)
	w.mu.Lock()
	defer w.mu.Unlock()
	if event.Job.Incremental {
		return w.acceptIncremental(event)
	}
	latest, ok := w.latest[source]
	if !ok || latest != event.Job.ticket {
		return false
	}
	delete(w.owed, source)
	delete(w.latest, source)
	return true
}

// Current checks the ticket before handling errors or scheduling dependency
// retries. A completed acknowledgement or newer job makes it false.
func (w *Worker) Current(event SearchEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if event.Job.Incremental {
		return w.currentIncremental(event)
	}
	latest, ok := w.latest[searchSlot(event.Job.Version.Source)]
	return ok && latest == event.Job.ticket
}
