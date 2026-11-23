package arb

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

type activeSearchPath struct {
	keys   []domain.PoolKey
	cancel context.CancelFunc
}

// Called under the worker mutex. Only explicitly changed pools advance their
// revisions; pools still owed a search must not invalidate unchanged work again.
func (w *Worker) submitIncremental(job SearchJob, source domain.StateSource) error {
	reset := false
	if previous := w.frames[source]; previous != nil && !sameSearchScope(previous.job.Version, job.Version) {
		reset = true
		if pending, ok := w.pending[source]; ok {
			supersedeSearch(pending)
			delete(w.pending, source)
		}
		w.retireFrame(source)
		w.runs[source].cancelAt(boundaryKind(previous.job.Version, job.Version), job.Dirty)
	}
	w.next++
	job.ticket, job.generation = w.next, w.generations[source]
	if w.changes[source] == nil {
		w.changes[source] = make(map[domain.PoolKey]uint64)
	}
	owed := w.owe(source)
	if reset {
		// The canceled jobs' short routes were acknowledged when ranked, and
		// their sizing ended with the lineage.
		for key := range owed.long {
			owed.short[key] = job.ticket
		}
	}
	for _, key := range job.Dirty {
		w.changes[source][key] = job.ticket
		owed.add(key, job.ticket)
	}
	for _, key := range job.Recheck {
		owed.add(key, job.ticket)
	}
	rechecked := owed.joinRechecks(job.ticket)
	for path := range w.paths[source] {
		if overlaps(path.keys, job.Dirty) {
			path.cancel()
		}
	}
	frame := ownFrame(job)
	if old := w.frames[source]; old != nil {
		old.release()
	}
	w.frames[source] = frame
	w.latest[source] = job.ticket
	// A heartbeat updates the publication context, without replacing active or
	// queued computations. Their proofs are checked against this newer cursor.
	_, queued := w.pending[source]
	_, retrying := w.retries[source]
	lane := w.runs[source]
	// Owed pools nobody is about to search: long routes wait for the job in
	// the long slot to end (resumeLong).
	recoverOwed := !queued && !retrying && lane.rank == nil && (len(owed.short) > 0 || len(owed.long) > 0 && lane.long == nil)
	if len(job.Dirty)+len(job.Recheck) == 0 && !rechecked && !recoverOwed && (!reset || owed.empty()) {
		if job.Observe != nil {
			job.Observe(SearchObservation{Outcome: SearchContinued})
		}
		return nil
	}
	if len(job.Dirty)+len(job.Recheck) > 0 || rechecked {
		owed.frames++
		if owed.since.IsZero() {
			owed.since = job.ReceivedAt
		}
	}
	if len(job.Dirty) > 0 {
		w.reviewRuns(source)
	}
	w.queueFrame(source, frame, frame.job)
	return nil
}

// queueFrame makes job, at frame, the lane's next one; take gives it the pools
// owed by then. The caller holds the worker mutex.
func (w *Worker) queueFrame(source domain.StateSource, frame *searchFrame, job SearchJob) {
	job.Release = frame.retain()
	if previous, ok := w.pending[source]; ok {
		supersedeSearch(previous)
	}
	job.queuedAt = time.Now()
	w.pending[source] = job
	w.wake(source)
}

// resumeLong queues a job at the latest frame for pools still owed their long
// routes once the long slot is free and no other job is about to take it.
// Without it they would wait for the next frame. The caller holds the worker
// mutex.
func (w *Worker) resumeLong(source domain.StateSource) {
	owed, frame, lane := w.owed[source], w.frames[source], w.runs[source]
	_, queued := w.pending[source]
	_, retrying := w.retries[source]
	if w.closed || owed == nil || len(owed.long) == 0 || frame == nil || queued || retrying || lane.rank != nil || lane.long != nil {
		return
	}
	// The frame's own job may be delivered already; this one is distinct.
	job := frame.job
	w.next++
	job.ticket, job.resumed = w.next, true
	w.queueFrame(source, frame, job)
}

func overlaps(a, b []domain.PoolKey) bool {
	for _, key := range a {
		if slices.Contains(b, key) {
			return true
		}
	}
	return false
}

func (w *Worker) wake(source domain.StateSource) {
	select {
	case w.wakes[source] <- struct{}{}:
	default:
	}
}

// Invalidation is a lineage boundary. It also retires prepared retries, whose
// readers remain independent of any actively executing evaluation.
func (w *Worker) retireFrame(source domain.StateSource) {
	if w.generations != nil {
		w.generations[source]++
	}
	if frame := w.frames[source]; frame != nil {
		frame.release()
	}
	delete(w.frames, source)
	delete(w.changes, source)
	if job, ok := w.retries[source]; ok {
		supersedeSearch(job)
		delete(w.retries, source)
	}
}

func (w *Worker) unchanged(job SearchJob, keys []domain.PoolKey) bool {
	source := searchSlot(job.Version.Source)
	frame := w.frames[source]
	if frame == nil || job.GraphVersion != w.graphVersion || job.generation != w.generations[source] || !sameSearchScope(job.Version, frame.job.Version) {
		return false
	}
	for _, key := range keys {
		if w.changes[source][key] > job.ticket {
			return false
		}
	}
	return true
}

func (w *Worker) currentIncremental(event SearchEvent) bool {
	return event.receipt != nil && !event.receipt.accepted.Load() && w.unchanged(event.receipt.origin, event.receipt.dependencies)
}

func (w *Worker) acceptIncremental(event SearchEvent) bool {
	if !w.currentIncremental(event) || !event.receipt.accepted.CompareAndSwap(false, true) {
		return false
	}
	// The worker acknowledged the job's pools when it ended (settle).
	return true
}

// Rebase runs on the state writer before cursor validation. The incoming event
// retains its original reader and gains a separate reference to the new frame.
func (w *Worker) Rebase(event *SearchEvent) bool {
	if !event.Job.Incremental {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.currentIncremental(*event) {
		return false
	}
	origin := event.receipt.origin
	frame := w.frames[searchSlot(origin.Version.Source)]
	if frame.job.Version == event.Job.Version {
		return true
	}
	keys := make(map[domain.PoolKey]struct{}, len(event.receipt.dependencies))
	for _, key := range event.receipt.dependencies {
		keys[key] = struct{}{}
	}
	event.Job.View = &carriedView{current: frame.job.View, basis: origin.View, keys: keys}
	event.Job.Version = frame.job.Version
	previous, current := event.Job.Release, frame.retain()
	event.Job.Release = func() { previous(); current() }
	return true
}

// Retry retains the original immutable computation basis. It must not publish
// an old frame as the latest cursor or merge unrelated newer dirty pools into it.
func (w *Worker) Retry(job SearchJob) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !job.Incremental || w.closed || !w.unchanged(job, job.dependencies) {
		return fmt.Errorf("%w: dependency retry is obsolete", ErrInvalidConfig)
	}
	if job.retryReceipt != nil && !job.retryReceipt.accepted.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: dependency retry already consumed", ErrInvalidConfig)
	}
	job.retryReceipt = nil
	source := searchSlot(job.Version.Source)
	if old, ok := w.retries[source]; ok {
		supersedeSearch(old)
	}
	job.queuedAt = time.Now()
	w.retries[source] = job
	w.wake(source)
	return nil
}

// PendingAfter excludes the batch whose final event the writer is consuming.
// A newer active batch can exist even when this acknowledgement clears dirty.
func (w *Worker) PendingAfter(event SearchEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	source := searchSlot(event.Job.Version.Source)
	_, pending := w.pending[source]
	_, retry := w.retries[source]
	ticket := event.Job.ticket
	if event.receipt != nil {
		ticket = event.receipt.origin.ticket
	}
	other := slices.ContainsFunc(w.runs[source].alive, func(run *searchRun) bool { return run.ticket != ticket })
	if owed := w.owed[source]; (owed != nil && !owed.empty()) || pending || retry || other {
		return true
	}
	w.deliveryMu.Lock()
	defer w.deliveryMu.Unlock()
	for receipt := range w.deliveries {
		if searchSlot(receipt.origin.Version.Source) == source && receipt != event.receipt && !receipt.accepted.Load() && w.unchanged(receipt.origin, receipt.dependencies) {
			return true
		}
	}
	return false
}

func (w *Worker) finalEvent(job SearchJob, result SearchResult, err error) SearchEvent {
	event := SearchEvent{Stages: job.Stages, Job: job, Result: result, Err: err}
	if job.Incremental {
		event.receipt = &searchReceipt{origin: job, dependencies: slices.Clone(job.dependencies)}
	}
	return w.trackDelivery(event)
}

// Delivery ownership includes queued events and their dependency-retry chain.
// A separate mutex permits release while Submit holds the scheduling mutex.
func (w *Worker) trackDelivery(event SearchEvent) SearchEvent {
	if event.receipt == nil {
		return event
	}
	w.deliveryMu.Lock()
	if w.deliveries == nil {
		w.deliveries = make(map[*searchReceipt]struct{})
	}
	w.deliveries[event.receipt] = struct{}{}
	w.deliveryMu.Unlock()
	previous, receipt := event.Job.Release, event.receipt
	event.Job.Release = func() {
		w.deliveryMu.Lock()
		delete(w.deliveries, receipt)
		w.deliveryMu.Unlock()
		previous()
	}
	return event
}
