package arb

import (
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// A frame can be owned by the latest cursor, an evaluator, and several emitted
// candidates. Canceling work never releases a reader before its actual exit.
type searchFrame struct {
	job   SearchJob
	refs  atomic.Int64
	close func()
}

func ownFrame(job SearchJob) *searchFrame {
	f := &searchFrame{job: job, close: job.Release}
	f.refs.Store(1)
	f.job.frame = f
	f.job.Release = f.release
	return f
}

func (f *searchFrame) retain() func() {
	f.refs.Add(1)
	return f.release
}

func (f *searchFrame) release() {
	if f.refs.Add(-1) == 0 && f.close != nil {
		f.close()
	}
}

type searchBatch struct{ firstAccepted, firstShortAccepted atomic.Bool }

type searchReceipt struct {
	origin       SearchJob
	dependencies []domain.PoolKey
	accepted     atomic.Bool
}

// OriginalJob supplies the immutable computation basis for a dependency retry.
// Its Release transfers the delivered event's ownership, including any rebase.
func (e SearchEvent) OriginalJob() SearchJob {
	if e.receipt == nil {
		return e.Job
	}
	job := e.receipt.origin
	job.retryReceipt = e.receipt
	job.Release = e.Job.Release
	return job
}

// A carried quote reads its proven, unchanged dependencies from the prepared
// basis, and everything else from the latest frame. No native getter runs here.
type carriedView struct {
	current, basis View
	keys           map[domain.PoolKey]struct{}
}

func (v *carriedView) Pool(key domain.PoolKey) (pool.State, bool) {
	if _, ok := v.keys[key]; ok {
		return v.basis.Pool(key)
	}
	return v.current.Pool(key)
}

func (v *carriedView) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	if _, ok := v.keys[key]; ok {
		return v.basis.Deep(key)
	}
	return v.current.Deep(key)
}

func (v *carriedView) SharedDeep(key domain.PoolKey) (pool.DeepState, bool) {
	if _, ok := v.keys[key]; ok {
		return sharedDeep(v.basis, key)
	}
	return sharedDeep(v.current, key)
}

func sameSearchScope(a, b domain.StateVersion) bool {
	return a.Source == b.Source && a.Epoch == b.Epoch && a.Anchor == b.Anchor && a.BlockNumber == b.BlockNumber
}

// MarkFirstCandidate is called after successful writer-side consumption, so a
// discarded earlier partial cannot hide the first usable candidate's latency.
func (e SearchEvent) MarkFirstCandidate() bool {
	if e.receipt == nil || e.receipt.origin.batch == nil {
		return e.First
	}
	return e.receipt.origin.batch.firstAccepted.CompareAndSwap(false, true)
}

// MarkFirstShortCandidate reports, once per job, a consumed candidate of at
// most shortRouteHops pools.
func (e SearchEvent) MarkFirstShortCandidate() bool {
	if e.receipt == nil || e.receipt.origin.batch == nil || len(e.Result.Opportunities) == 0 || len(e.Result.Opportunities[0].Cycle.Pools) > shortRouteHops {
		return false
	}
	return e.receipt.origin.batch.firstShortAccepted.CompareAndSwap(false, true)
}
