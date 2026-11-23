package arb

import (
	"context"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
)

// laneRunLimit bounds the jobs of one source that run at once: one ranking
// long routes, one ranking short routes, and two still sizing the short
// routes they ranked. Each holds its frame's reader and an exact pipeline.
// With three, the job in the long slot left room for one job sizing, and new
// frames waited for it: in a synthetic 150-frame burst with -cpu 7
// (BenchmarkBurstBlock, 5 blocks each) the first short quote of
// the frames that got one came after 12.0 ms (p50) with three, 10.7 ms with
// four and 10.2 ms with six.
const laneRunLimit = 4

// searchRun is one running job of a source lane. A streamed incremental job
// has two phases. The short phase ranks every route of at most shortRouteHops
// pools through the pools owed their short routes, then sizes the selection.
// The long phase ranks the longer routes and the catalog paths of the pools
// owed theirs, once the job's own short quotes are out.
//
// Short ranking holds the lane's rank slot and long ranking its long slot, one
// job each. The lane starts the next job when the rank slot is free, so a new
// frame's short routes wait for the previous job's short ranking and never
// for its sizing or for long routes. A job that finds the long slot taken
// ends after its short routes; its pools stay owed their long routes
// (owedPools) and the next job to take the slot ranks them at its own frame.
//
// A newer frame never cancels a run for arriving. It cancels the ranking of a
// phase only when it has changed every pool that phase ranks through
// (triggers): each route the phase would still rank then holds a changed pool
// and would be dropped. Routes a newer frame changed are otherwise dropped
// path by path (beginPath, pathCurrent, Rebase), and the changed pools are
// owed a search again. If a queued frame canceled the long-route rotation as
// soon as the first short batches were ranked, the canceled job would
// acknowledge none of its pools: under bursty Fast Feed load only about a
// third of searches complete that way, and none within a 150-message block.
type searchRun struct {
	ticket uint64
	// cancel ends the whole job at a block, epoch, graph or shutdown boundary.
	cancel context.CancelFunc
	// coarse cancels the ranking in progress; sizing already queued is
	// canceled per path. triggers are the pools that ranking goes through.
	coarse   context.CancelFunc
	triggers []domain.PoolKey
	// left is closed once the run no longer holds the rank slot.
	left                  chan struct{}
	leave                 sync.Once
	overtaken, overlapped bool
	// boundary is set when a lineage boundary cancels the run.
	boundary SearchBoundary
}

// SearchBoundary describes the lineage boundary that canceled a search: its
// kind, and whether the frame that brought it changed the pools the search
// was ranking through. Measurement only.
type SearchBoundary struct {
	Kind    BoundaryKind
	Reached BoundaryReach
}

type BoundaryKind uint8

const (
	BoundaryNone BoundaryKind = iota
	BoundaryBlock
	BoundaryAnchor
	BoundaryEpoch
)

type BoundaryReach uint8

const (
	// ReachSizing: the run had ranked and was sizing; its pools are not kept.
	ReachSizing BoundaryReach = iota
	ReachUntouched
	ReachTouched
)

func boundaryKind(previous, next domain.StateVersion) BoundaryKind {
	switch {
	case previous.Epoch != next.Epoch:
		return BoundaryEpoch
	case previous.BlockNumber != next.BlockNumber:
		return BoundaryBlock
	}
	return BoundaryAnchor
}

type laneRuns struct {
	rank, long *searchRun
	alive      []*searchRun
	// newest is the ticket of the last job started; its short routes are
	// sized ahead of earlier jobs' (cpuBacklog).
	newest atomic.Uint64
}

func (l *laneRuns) cancel() {
	for _, run := range l.alive {
		run.cancel()
	}
}

// cancelAt cancels every run at a lineage boundary and records, for the runs
// still ranking, whether the boundary's frame changed their pools. The caller
// holds the worker mutex.
func (l *laneRuns) cancelAt(kind BoundaryKind, dirty []domain.PoolKey) {
	for _, run := range l.alive {
		if run.boundary.Kind == BoundaryNone {
			run.boundary = SearchBoundary{Kind: kind}
			if run.coarse != nil {
				run.boundary.Reached = ReachUntouched
				if overlaps(run.triggers, dirty) {
					run.boundary.Reached = ReachTouched
				}
			}
		}
		run.cancel()
	}
}

// beginCoarse makes a ranking through triggers cancelable by newer frames.
// It reports false, and cancels, when frames have already changed them all.
func (w *Worker) beginCoarse(source domain.StateSource, run *searchRun, triggers []domain.PoolKey, cancel context.CancelFunc) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	run.coarse, run.triggers = cancel, triggers
	if w.triggersChanged(source, run) {
		run.overlapped = true
		cancel()
		return false
	}
	return true
}

// endCoarse runs when a ranking is over: later frames reach the job's queued
// sizing path by path.
func (w *Worker) endCoarse(job *SearchJob, run *searchRun) {
	w.mu.Lock()
	defer w.mu.Unlock()
	run.coarse, run.triggers = nil, nil
	job.overtaken, job.overlapped = run.overtaken, run.overlapped
}

// reviewRuns runs under the worker mutex when a frame changed pool inputs.
func (w *Worker) reviewRuns(source domain.StateSource) {
	for _, run := range w.runs[source].alive {
		if run.coarse == nil {
			continue
		}
		if !w.triggersChanged(source, run) {
			run.overtaken = true
			continue
		}
		run.overlapped = true
		run.coarse()
	}
}

func (w *Worker) triggersChanged(source domain.StateSource, run *searchRun) bool {
	if len(run.triggers) == 0 {
		return false
	}
	for _, key := range run.triggers {
		if w.changes[source][key] <= run.ticket {
			return false
		}
	}
	return true
}

// leaveRank frees the rank slot for the lane's next job.
func (w *Worker) leaveRank(source domain.StateSource, run *searchRun) {
	w.mu.Lock()
	if lane := w.runs[source]; lane.rank == run {
		lane.rank = nil
	}
	w.mu.Unlock()
	run.leave.Do(func() { close(run.left) })
}

// enterLong gives the job the long slot and the pools owed their long routes
// as of its frame, or reports that an earlier job still holds the slot.
func (w *Worker) enterLong(job *SearchJob, run *searchRun) bool {
	source := searchSlot(job.Version.Source)
	w.mu.Lock()
	defer w.mu.Unlock()
	lane := w.runs[source]
	if lane.long != nil {
		return false
	}
	lane.long = run
	if owed := w.owed[source]; owed != nil && job.generation == w.generations[source] {
		job.longTriggers = owed.longUpTo(job.ticket)
	}
	job.Dirty = append(slices.Clone(job.shortTriggers), job.longTriggers...)
	slices.SortFunc(job.Dirty, domain.ComparePoolKeys)
	job.Dirty = slices.Compact(job.Dirty)
	return true
}

// retire frees the run's slots. After a job that completed, pools left owed
// their long routes get a job of their own; a failed job is not repeated.
func (w *Worker) retire(source domain.StateSource, run *searchRun, completed bool) {
	w.mu.Lock()
	lane := w.runs[source]
	if lane.rank == run {
		lane.rank = nil
	}
	if lane.long == run {
		lane.long = nil
	}
	lane.alive = slices.DeleteFunc(lane.alive, func(other *searchRun) bool { return other == run })
	if completed {
		w.resumeLong(source)
	}
	w.mu.Unlock()
	run.leave.Do(func() { close(run.left) })
}
