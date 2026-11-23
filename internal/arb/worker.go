package arb

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// SearchJob owns an immutable view. Standard prefixes and verified standard
// blocks share a scheduling slot; Fast prefixes have their own slot.
type SearchJob struct {
	Stages       SearchStages
	exactReadyAt time.Time
	initials     *progressQuotes
	// Incremental enables dependency-scoped cancellation and candidate streaming.
	Incremental bool
	// QuoteBasis describes the state evidence for unconfirmed paper quotes.
	QuoteBasis string
	View       View
	Dirty      []domain.PoolKey
	// Recheck requests fresh publication without claiming pool inputs changed.
	Recheck      []domain.PoolKey
	Version      domain.StateVersion
	GraphVersion uint64
	// ReceivedAt is the optional local trigger time for end-to-end telemetry.
	ReceivedAt time.Time
	// FirstReceivedAt is the ReceivedAt of the oldest frame whose pools this
	// job searches; the worker sets it when the job starts.
	FirstReceivedAt time.Time
	// Observe runs on the scheduler/evaluator and must be concurrency-safe and
	// nonblocking. It is removed before publishing a result.
	Observe func(SearchObservation)
	// Prepare refreshes version-bound dependencies on the evaluator, using the
	// accumulated dirty set. It must own its captured state and honor ctx.
	Prepare func(context.Context, View, []domain.PoolKey) (View, []QuoteIssue, error)
	// PrepareExact runs for selected path dependencies. Streaming discovery may
	// call it in disjoint batches before ranking completes; View carries prior
	// preparation forward so a pool is not fetched twice in one job.
	// It shares Prepare's snapshot and Release ownership; cancellation must not
	// close readers until this callback and all exact quotes actually exit.
	PrepareExact func(context.Context, View, []domain.PoolKey) (View, []QuoteIssue, error)
	// Release owns captured reader resources. After a successful Submit the
	// worker releases superseded/canceled jobs; a delivered event transfers
	// ownership to its consumer. It must be nonblocking and concurrency-safe.
	Release      func()
	frame        *searchFrame
	generation   uint64
	dependencies []domain.PoolKey
	retryReceipt *searchReceipt
	batch        *searchBatch
	ticket       uint64
	engine       *Engine // captured under the worker lock when evaluation starts
	queuedAt     time.Time
	// Copied from the job's searchRun when ranking ends.
	overtaken, overlapped bool
	// shortTriggers and longTriggers are the pools whose short and long
	// routes this job ranks; Dirty is their union. A phased job ranks them in
	// two phases and may leave the long routes to a later job (searchRun).
	shortTriggers, longTriggers []domain.PoolKey
	phased                      bool
	// resumed marks a job the worker queued for long routes left owed.
	resumed bool
	long    LongOutcome
	frames  int
}

type SearchOutcome uint8

const (
	SearchCompleted SearchOutcome = iota
	SearchCanceled
	SearchSuperseded
	SearchContinued
)

// LongOutcome is what became of a streamed search's long routes, those of more
// than shortRouteHops pools.
type LongOutcome uint8

const (
	// LongNone: the job does not rank short and long routes in separate phases.
	LongNone LongOutcome = iota
	LongCompleted
	// LongDeferred: an earlier search was still ranking long routes, so the
	// pools stay owed theirs.
	LongDeferred
	// LongPreempted: newer frames changed every pool the long routes were
	// ranked through; the pools stay owed theirs.
	LongPreempted
)

type SearchObservation struct {
	Stages                   SearchStages
	QueueWait                time.Duration
	Evaluation               time.Duration
	Outcome                  SearchOutcome
	Failed                   bool
	PathCanceled, PathReused uint64
	// Overtaken: a newer frame with changed pools arrived while the search
	// ranked and left it running. Overlapped: newer frames changed every pool
	// the search was ranking through, so its ranking was canceled.
	Overtaken, Overlapped bool
	// Frames is how many frames with changed or rechecked pools the search
	// absorbed: its own and those that queued while earlier searches ran.
	Frames int
	Long   LongOutcome
	// Resumed: the worker queued this search for long routes left owed.
	Resumed bool
	// Boundary is set on a search a lineage boundary canceled.
	Boundary SearchBoundary
}

func supersedeSearch(job SearchJob) {
	if job.Observe != nil {
		stages := SearchStages{Queue: time.Since(job.queuedAt)}
		if !job.ReceivedAt.IsZero() {
			stages.Intake = max(0, job.queuedAt.Sub(job.ReceivedAt))
		}
		job.Observe(SearchObservation{Stages: stages, QueueWait: stages.Queue, Outcome: SearchSuperseded})
	}
	releaseSearch(job)
}

func releaseSearch(job SearchJob) {
	if job.Release != nil {
		job.Release()
	}
}

type SearchEvent struct {
	Stages SearchStages
	Stage  QuoteStage
	// QuotedAt precedes publication backpressure and consumer queueing.
	QuotedAt time.Time
	// Partial carries one completed candidate; the final event owns diagnostics.
	Partial bool
	First   bool
	receipt *searchReceipt
	Job     SearchJob
	Result  SearchResult
	Err     error
}

// Worker has independent standard/Fast evaluators and latest-job slots. Dirty
// pools stay owed a search until one covers them (owedPools); superseding a
// job never loses earlier dirty pools. Submit and Invalidate are nonblocking
// with respect to evaluation. Quoters and option callbacks must support
// searches running concurrently, up to laneRunLimit per source (searchRun);
// quoters must also honor their context.
type Worker struct {
	cpus         *searchCPUs
	deliveryMu   sync.Mutex
	deliveries   map[*searchReceipt]struct{}
	mu           sync.Mutex
	engine       *Engine
	graphVersion uint64
	opts         RankedOptions
	pending      map[domain.StateSource]SearchJob
	owed         map[domain.StateSource]*owedPools
	latest       map[domain.StateSource]uint64
	runs         map[domain.StateSource]*laneRuns
	next         uint64
	wakes        map[domain.StateSource]chan struct{}
	results      chan SearchEvent
	done         chan struct{}
	closed       bool
	frames       map[domain.StateSource]*searchFrame
	changes      map[domain.StateSource]map[domain.PoolKey]uint64
	generations  map[domain.StateSource]uint64
	retries      map[domain.StateSource]SearchJob
	paths        map[domain.StateSource]map[*activeSearchPath]struct{}
}

func NewWorker(ctx context.Context, engine *Engine, opts RankedOptions) (*Worker, error) {
	if engine == nil || opts.Bounds == nil || opts.SwapFee == nil {
		return nil, fmt.Errorf("%w: worker engine or search options missing", ErrInvalidConfig)
	}
	w := &Worker{
		engine: engine, graphVersion: 1, opts: opts,
		pending: make(map[domain.StateSource]SearchJob), latest: make(map[domain.StateSource]uint64),
		owed:    make(map[domain.StateSource]*owedPools),
		wakes:   map[domain.StateSource]chan struct{}{domain.SourceStandard: make(chan struct{}, 1), domain.SourceFast: make(chan struct{}, 1)},
		runs:    map[domain.StateSource]*laneRuns{domain.SourceStandard: {}, domain.SourceFast: {}},
		results: make(chan SearchEvent, 2), done: make(chan struct{}),
	}
	w.frames = make(map[domain.StateSource]*searchFrame)
	w.changes = make(map[domain.StateSource]map[domain.PoolKey]uint64)
	w.generations = make(map[domain.StateSource]uint64)
	w.retries = make(map[domain.StateSource]SearchJob)
	w.paths = make(map[domain.StateSource]map[*activeSearchPath]struct{})
	w.cpus = newSearchCPUsOf(runtime.GOMAXPROCS(0) - max(0, opts.ReservedCPUs))
	go w.run(ctx)
	return w, nil
}

func searchSlot(source domain.StateSource) domain.StateSource {
	if source == domain.SourceStandardPrefix {
		return domain.SourceStandard
	}
	return source
}

// Invalidate is a lineage boundary: it drops the pending, latest and retry
// state of source and cancels its active search. It returns the pools whose
// search had not completed, for a caller that resumes the same lineage later,
// such as catch-up to the tip.
func (w *Worker) Invalidate(source domain.StateSource) []domain.PoolKey {
	source = searchSlot(source)
	w.mu.Lock()
	defer w.mu.Unlock()
	w.retireFrame(source)
	if pending, ok := w.pending[source]; ok {
		supersedeSearch(pending)
	}
	var dropped []domain.PoolKey
	if owed := w.owed[source]; owed != nil {
		dropped = owed.searchable()
	}
	delete(w.pending, source)
	delete(w.latest, source)
	delete(w.owed, source)
	if lane := w.runs[source]; lane != nil {
		lane.cancel()
	}
	return dropped
}

// ReplaceEngine installs an already-built immutable topology. Old jobs and
// results lose their tickets immediately; the active evaluator is canceled
// without waiting for it. Evaluation keeps its captured engine until it exits.
func (w *Worker) ReplaceEngine(engine *Engine, version uint64) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if engine == nil || version <= w.graphVersion {
		return fmt.Errorf("%w: replacement graph must have an engine and a newer version", ErrInvalidConfig)
	}
	w.engine, w.graphVersion = engine, version
	for source := range w.frames {
		w.retireFrame(source)
	}
	for _, pending := range w.pending {
		supersedeSearch(pending)
	}
	clear(w.pending)
	clear(w.latest)
	clear(w.owed)
	for _, lane := range w.runs {
		lane.cancel()
	}
	return nil
}

func (w *Worker) Results() <-chan SearchEvent { return w.results }
func (w *Worker) Done() <-chan struct{}       { return w.done }

func (w *Worker) run(ctx context.Context) {
	var lanes sync.WaitGroup
	lanes.Go(func() { w.maintainPaths(ctx) })
	for _, source := range []domain.StateSource{domain.SourceStandard, domain.SourceFast} {
		lanes.Go(func() { w.runLane(ctx, source) })
	}
	<-ctx.Done()
	w.mu.Lock()
	w.closed = true
	for source := range w.frames {
		w.retireFrame(source)
	}
	for _, pending := range w.pending {
		supersedeSearch(pending)
	}
	clear(w.pending)
	for _, lane := range w.runs {
		lane.cancel()
	}
	w.mu.Unlock()
	lanes.Wait() // Cancellation never releases a reader that is still executing.
	close(w.results)
	for event := range w.results {
		releaseSearch(event.Job)
	}
	close(w.done)
}
