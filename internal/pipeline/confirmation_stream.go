package pipeline

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/telemetry"
)

// Offer queues individual candidates without canceling earlier candidates from
// the same prefix. The cap covers active, queued and completed offers together.
// Ownership transfers only when accepted. A new prefix still retires old work.
func (w *ConfirmationWorker) Offer(job ConfirmationJob, limit int) (bool, error) {
	if job.Task == nil || job.Identity.Graph == 0 || w.timeout <= 0 || limit <= 0 {
		return false, fmt.Errorf("candidate confirmation requires task, graph and positive limits")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return false, fmt.Errorf("confirmation worker stopped")
	}
	if w.offerRetired == nil || w.offerVersion != job.Identity.Version || w.offerGraph != job.Identity.Graph {
		w.next++
		w.retirePending()
		w.retireOffers()
		if w.activeCancel != nil {
			w.activeCancel()
		}
		w.offerVersion, w.offerGraph = job.Identity.Version, job.Identity.Graph
		w.offerRetired = make(chan struct{})
	}
	if w.offerCount >= limit {
		return false, nil
	}
	w.offerCount++
	job.Identity.ticket = w.next
	w.nextTask++
	job.serial = w.nextTask
	job.queuedAt = w.metrics.Start()
	job.retired = w.offerRetired
	w.metrics.Add(MetricSource(job.Identity.Version.Source), telemetry.ConfirmationSubmitted, 1)
	w.queued = append(w.queued, &job)
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return true, nil
}

func (w *ConfirmationWorker) retireOffers() {
	if w.offerRetired != nil {
		close(w.offerRetired)
		w.offerRetired = nil
	}
	for _, job := range w.queued {
		job.Task.Close()
	}
	clear(w.queued)
	w.queued = w.queued[:0]
	w.offerCount = 0
}
