package arb

import (
	"maps"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// owedPools are the pools of one source whose routes still need a search at
// the current lineage. A changed pool is owed both its short and its long
// routes. The worker clears a pool when a job has ranked those routes and no
// later frame changed the pool (settle); a job that left its long routes to a
// later one clears only short.
//
// Acknowledgement used to wait for the writer to accept the job's final
// event, which requires that no pool on any route through the job's pools
// changed meanwhile. With a million routes almost every pool shares a route
// with a hot one, so under a stream of frames no search was acknowledged and
// each job searched every pool changed since the block began. Candidates
// carry their own per-pool proof, and one the writer drops although its pools
// are unchanged comes back through Recheck.
type owedPools struct {
	// short and long hold, per pool, the ticket of the latest frame that
	// changed the pool or asked for its routes again.
	short, long map[domain.PoolKey]uint64
	// recheckShort and recheckLong join short and long with the next frame.
	recheckShort, recheckLong map[domain.PoolKey]struct{}
	// frames counts the frames that added pools since a job last took them,
	// and since is the oldest of their arrivals.
	frames int
	since  time.Time
}

// owe returns source's record. The caller holds the worker mutex.
func (w *Worker) owe(source domain.StateSource) *owedPools {
	owed := w.owed[source]
	if owed == nil {
		owed = &owedPools{short: make(map[domain.PoolKey]uint64), long: make(map[domain.PoolKey]uint64),
			recheckShort: make(map[domain.PoolKey]struct{}), recheckLong: make(map[domain.PoolKey]struct{})}
		w.owed[source] = owed
	}
	return owed
}

func (o *owedPools) add(key domain.PoolKey, ticket uint64) {
	o.short[key] = ticket
	o.long[key] = ticket
}

func (o *owedPools) empty() bool { return len(o.short)+len(o.long) == 0 }

// searchable lists every owed pool in key order.
func (o *owedPools) searchable() []domain.PoolKey {
	keys := slices.AppendSeq(slices.Collect(maps.Keys(o.short)), maps.Keys(o.long))
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return slices.Compact(keys)
}

// longUpTo lists, in key order, the pools owed their long routes by frames up
// to ticket: a job's view does not hold later changes.
func (o *owedPools) longUpTo(ticket uint64) []domain.PoolKey {
	var keys []domain.PoolKey
	for key, at := range o.long {
		if at <= ticket {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return keys
}

// claim gives job the pools owed their short routes and the frames that
// brought them. A job that ranks everything in one pass (phased false) takes
// the pools owed long routes too. The caller holds the worker mutex.
func (o *owedPools) claim(job *SearchJob, phased bool) {
	job.shortTriggers = slices.SortedFunc(maps.Keys(o.short), domain.ComparePoolKeys)
	job.Dirty = job.shortTriggers
	if !phased {
		job.Dirty = o.searchable()
		job.shortTriggers, job.longTriggers = job.Dirty, job.Dirty
	}
	job.frames, job.FirstReceivedAt = o.frames, o.since
	if job.FirstReceivedAt.IsZero() {
		job.FirstReceivedAt = job.ReceivedAt
	}
	o.frames, o.since = 0, time.Time{}
}

// settle acknowledges routes job ranked, for every pool no later frame has
// changed or asked for again: the short routes once the short phase has
// delivered or dropped every quote, the long routes when the job ends having
// ranked them.
func (w *Worker) settle(job *SearchJob, long bool) {
	source := searchSlot(job.Version.Source)
	w.mu.Lock()
	defer w.mu.Unlock()
	owed := w.owed[source]
	if owed == nil || job.generation != w.generations[source] || job.GraphVersion != w.graphVersion {
		return
	}
	pools, keys := owed.short, job.shortTriggers
	if long {
		pools, keys = owed.long, job.longTriggers
	}
	for _, key := range keys {
		if pools[key] <= job.ticket {
			delete(pools, key)
		}
	}
}

// Recheck takes a candidate the writer dropped without consuming it, for a
// reason other than its own pools: input was queued behind it, or the cursor
// had moved. If its pools are still unchanged nothing else would search the
// route again, so one of them joins the next frame's search. It reports
// whether the route was taken back.
func (w *Worker) Recheck(event SearchEvent) bool {
	if !event.Partial || event.receipt == nil || len(event.receipt.dependencies) == 0 {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.currentIncremental(event) {
		return false
	}
	owed := w.owe(searchSlot(event.receipt.origin.Version.Source))
	key := event.receipt.dependencies[0]
	if routeClass(len(event.receipt.dependencies)) == cpuUrgent {
		owed.recheckShort[key] = struct{}{}
	} else {
		owed.recheckLong[key] = struct{}{}
	}
	return true
}

// joinRechecks moves rechecked pools into the owed sets at the frame of
// ticket. The caller holds the worker mutex.
func (o *owedPools) joinRechecks(ticket uint64) bool {
	joined := len(o.recheckShort)+len(o.recheckLong) > 0
	for key := range o.recheckShort {
		o.short[key] = ticket
	}
	for key := range o.recheckLong {
		o.long[key] = ticket
	}
	clear(o.recheckShort)
	clear(o.recheckLong)
	return joined
}
