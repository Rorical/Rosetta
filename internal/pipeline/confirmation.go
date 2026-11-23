// Package pipeline coordinates chain-independent search and confirmation work.
// Native state, transport decoding and persistence remain owned by backends.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// ConfirmationTask owns an immutable backend snapshot. Cancellation does not
// release it: Close runs only after Run returns, or if queued work is retired.
// Close must be nonblocking and must not invalidate any other retained snapshot.
type ConfirmationTask interface {
	Run(context.Context) (*execution.ReadyPlan, []string, error)
	Close()
}

type ConfirmationIdentity struct {
	Version    domain.StateVersion
	Graph      uint64
	ReceivedAt time.Time
	ticket     uint64
}

type ConfirmationJob struct {
	serial   uint64
	Identity ConfirmationIdentity
	Task     ConfirmationTask
	queuedAt time.Time
	retired  <-chan struct{}
}

// ConfirmationEvent carries no task or native reader ownership.
type ConfirmationEvent struct {
	serial     uint64
	Identity   ConfirmationIdentity
	Plan       *execution.ReadyPlan
	Rejections []string
	Err        error
}

// ConfirmationWorker supports latest-only batches and bounded candidate FIFO
// within one prefix. Canonical and tentative branches use separate workers.
// Submit/Invalidate never wait for an executing task to exit.
type ConfirmationWorker struct {
	nextTask, activeSerial, publishingSerial uint64
	mu                                       sync.Mutex
	pending                                  *ConfirmationJob
	next                                     uint64
	activeCancel                             context.CancelFunc
	wake                                     chan struct{}
	results                                  chan ConfirmationEvent
	done                                     chan struct{}
	timeout                                  time.Duration
	metrics                                  *telemetry.Registry
	closed                                   bool
	queued                                   []*ConfirmationJob
	offerVersion                             domain.StateVersion
	offerGraph                               uint64
	offerCount                               int
	offerRetired                             chan struct{}
}

func NewConfirmationWorker(ctx context.Context, timeout time.Duration) *ConfirmationWorker {
	w := &ConfirmationWorker{timeout: timeout, metrics: telemetry.FromContext(ctx), wake: make(chan struct{}, 1), results: make(chan ConfirmationEvent, 1), done: make(chan struct{})}
	go w.run(ctx)
	return w
}

// Submit transfers ownership only on success. The caller closes a rejected task.
func (w *ConfirmationWorker) Submit(job ConfirmationJob) error {
	if job.Task == nil || job.Identity.Graph == 0 || w.timeout <= 0 {
		return fmt.Errorf("confirmation requires task, graph version and positive timeout")
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return fmt.Errorf("confirmation worker stopped")
	}
	w.next++
	w.retireOffers()
	w.retirePending()
	job.Identity.ticket = w.next
	w.nextTask++
	job.serial = w.nextTask
	job.queuedAt = w.metrics.Start()
	w.metrics.Add(MetricSource(job.Identity.Version.Source), telemetry.ConfirmationSubmitted, 1)
	w.pending = &job
	if w.activeCancel != nil {
		w.activeCancel()
	}
	select {
	case w.wake <- struct{}{}:
	default:
	}
	return nil
}

func (w *ConfirmationWorker) Invalidate() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.next++
	w.retireOffers()
	w.retirePending()
	if w.activeCancel != nil {
		w.activeCancel()
	}
}

func (w *ConfirmationWorker) retirePending() {
	if w.pending == nil {
		return
	}
	job := w.pending
	w.pending = nil
	source := MetricSource(job.Identity.Version.Source)
	w.metrics.Add(source, telemetry.ConfirmationSuperseded, 1)
	w.metrics.Since(source, telemetry.ConfirmationQueue, job.queuedAt)
	job.Task.Close()
}

func (w *ConfirmationWorker) Current(event ConfirmationEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return event.Identity.ticket != 0 && event.Identity.ticket == w.next
}

// Backlog counts accepted work that has not started, for metric sampling.
func (w *ConfirmationWorker) Backlog() int64 {
	w.mu.Lock()
	defer w.mu.Unlock()
	count := int64(len(w.queued))
	if w.pending != nil {
		count++
	}
	return count
}
func (w *ConfirmationWorker) Busy() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.activeCancel != nil
}
func (w *ConfirmationWorker) Results() <-chan ConfirmationEvent { return w.results }
func (w *ConfirmationWorker) Done() <-chan struct{}             { return w.done }

func (w *ConfirmationWorker) run(ctx context.Context) {
	defer close(w.done)
	defer close(w.results)
	defer func() { w.mu.Lock(); defer w.mu.Unlock(); w.closed = true; w.retirePending(); w.retireOffers() }()
	for {
		select {
		case <-ctx.Done():
			return
		case <-w.wake:
		}
		for ctx.Err() == nil {
			w.mu.Lock()
			job := w.pending
			if job == nil && len(w.queued) > 0 {
				job = w.queued[0]
				w.queued[0] = nil
				w.queued = w.queued[1:]
			}
			if job == nil {
				w.mu.Unlock()
				break
			}
			w.pending = nil
			jobCtx, cancel := context.WithTimeout(ctx, w.timeout)
			jobCtx = telemetry.WithSource(jobCtx, MetricSource(job.Identity.Version.Source))
			w.activeCancel = cancel
			w.activeSerial = job.serial
			w.mu.Unlock()
			started := w.metrics.Start()
			source := MetricSource(job.Identity.Version.Source)
			w.metrics.Observe(source, telemetry.ConfirmationQueue, started.Sub(job.queuedAt))
			plan, rejected, err := job.Task.Run(jobCtx)
			job.Task.Close()
			job.Task = nil
			w.metrics.Since(source, telemetry.ConfirmationBatch, started)
			counter := telemetry.ConfirmationCompleted
			if jobCtx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				counter = telemetry.ConfirmationCanceled
			}
			if err != nil && counter != telemetry.ConfirmationCanceled {
				w.metrics.Add(source, telemetry.ConfirmationErrors, 1)
			}
			w.metrics.Add(source, counter, 1)
			w.metrics.Add(source, telemetry.ConfirmationRejected, uint64(len(rejected)))
			cancel()
			w.mu.Lock()
			w.activeCancel = nil
			w.activeSerial = 0
			current := job.Identity.ticket == w.next
			if current {
				w.publishingSerial = job.serial
			}
			w.mu.Unlock()
			if !current {
				w.metrics.Add(source, telemetry.PlanDiscarded, 1)
				continue
			}
			event := ConfirmationEvent{serial: job.serial, Identity: job.Identity, Plan: plan, Rejections: rejected, Err: err}
			if job.retired == nil {
				select {
				case <-w.results:
					w.metrics.Add(source, telemetry.PlanDiscarded, 1)
				default:
				}
			}
			select {
			case w.results <- event:
			case <-job.retired:
			case <-ctx.Done():
				return
			}
			w.mu.Lock()
			if w.publishingSerial == job.serial {
				w.publishingSerial = 0
			}
			w.mu.Unlock()
		}
	}
}

// PendingAfter excludes the result currently being consumed, including the
// brief overlap where its sender has not yet finished publishing it.
func (w *ConfirmationWorker) PendingAfter(event ConfirmationEvent) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.pending != nil || len(w.queued) > 0 || len(w.results) > 0 ||
		(w.activeCancel != nil && w.activeSerial != event.serial) ||
		(w.publishingSerial != 0 && w.publishingSerial != event.serial)
}
