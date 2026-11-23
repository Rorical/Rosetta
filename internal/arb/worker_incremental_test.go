package arb

import (
	"context"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

type pathGateQuoter struct {
	countPool  domain.PoolKey
	firstCalls atomic.Int64
	block      domain.PoolKey
	entered    chan struct{}
	finish     chan struct{}
	canceled   chan struct{}
	once       sync.Once
}

func (q *pathGateQuoter) QuoteExactIn(ctx context.Context, p pool.State, _ pool.DeepState, token domain.Address, input *big.Int) (*big.Int, error) {
	if p.Key == q.countPool {
		q.firstCalls.Add(1)
	}
	if p.Key == q.block {
		blocked := false
		q.once.Do(func() { close(q.entered); blocked = true })
		if blocked {
			select {
			case <-q.finish:
			case <-ctx.Done():
				close(q.canceled)
				return nil, ctx.Err()
			}
		}
	}
	in, out, _ := traversalReserves(p, token)
	return math.QuoteV2ExactIn(in, out, p.Fee, input)
}

func incrementalFixture(t *testing.T, blockSecond bool) (*Worker, SearchJob, *pathGateQuoter, context.CancelFunc) {
	t.Helper()
	pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000), testV2State(3, 1, 3, 1000000, 1500000), testV2State(4, 3, 1, 1000000, 1500000)}
	block := pools[0].Key
	if blockSecond {
		block = pools[2].Key
	}
	q := &pathGateQuoter{countPool: pools[0].Key, block: block, entered: make(chan struct{}), finish: make(chan struct{}), canceled: make(chan struct{})}
	engine := mustEngine(t, explicitTable(t, pools[0].Token0, []domain.PoolKey{pools[0].Key, pools[1].Key}, []domain.PoolKey{pools[2].Key, pools[3].Key}), 1000, 2, q)
	ctx, cancel := context.WithCancel(t.Context())
	w, err := NewWorker(ctx, engine, rankedOptions(pools[0].Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	view := newFakeView()
	for _, p := range pools {
		view.add(p)
	}
	job := SearchJob{Incremental: true, View: view, Dirty: []domain.PoolKey{pools[0].Key, pools[2].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 10}}
	t.Cleanup(func() { cancel(); <-w.Done() })
	return w, job, q, cancel
}

func waitSignal(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach gate")
	}
}
func nextSearch(t *testing.T, w *Worker) SearchEvent {
	t.Helper()
	select {
	case e := <-w.Results():
		return e
	case <-time.After(5 * time.Second):
		t.Fatal("worker produced no event")
		return SearchEvent{}
	}
}

func TestIncrementalHeartbeatKeepsActivePathAndRebasesResult(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, false)
	var closes atomic.Int32
	job.Release = func() { closes.Add(1) }
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.entered)
	next := job
	next.Dirty = nil
	next.Version.TxIndex = 1
	next.Release = nil
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	select {
	case <-q.canceled:
		t.Fatal("heartbeat canceled exact path")
	default:
	}
	if closes.Load() != 0 {
		t.Fatal("heartbeat released executing reader")
	}
	close(q.finish)
	var final SearchEvent
	for {
		e := nextSearch(t, w)
		if !w.Rebase(&e) || e.Job.Version != next.Version || !w.Accept(e) {
			t.Fatal("unchanged quote failed current-frame rebase")
		}
		releaseSearch(e.Job)
		if !e.Partial {
			final = e
			break
		}
	}
	if closes.Load() != 1 || w.PendingAfter(final) {
		t.Fatal("reader or dirty acknowledgement leaked", closes.Load())
	}
}

func TestIncrementalChangedPathCancelsWithoutRestartingNeighbour(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, true)
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	first := nextSearch(t, w)
	if !first.Partial || len(first.Result.Opportunities) != 1 {
		t.Fatal("first candidate waited for whole batch")
	}
	waitSignal(t, q.entered)
	quoted := q.firstCalls.Load()
	next := job
	next.Dirty = []domain.PoolKey{q.block}
	next.Version.TxIndex = 1
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.canceled)
	if !w.Rebase(&first) || !w.Accept(first) {
		t.Fatal("unrelated pool invalidated completed neighbour")
	}
	releaseSearch(first.Job)
	// A newly changed dependency retires an already queued quote as well.
	firstAgain := first
	if w.Current(firstAgain) {
		t.Fatal("accepted receipt was reusable")
	}
	for i := 0; i < 8; i++ {
		e := nextSearch(t, w)
		valid := w.Rebase(&e)
		if valid {
			w.Accept(e)
		}
		final := valid && !e.Partial
		releaseSearch(e.Job)
		if final {
			if q.firstCalls.Load() != quoted {
				t.Fatal("unchanged neighbour was quoted again", quoted, q.firstCalls.Load())
			}
			return
		}
	}
	t.Fatal("latest changed path did not finish")
}

func TestIncrementalQueuedCandidateRejectedAfterDependencyChange(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, true)
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	first := nextSearch(t, w)
	waitSignal(t, q.entered)
	next := job
	next.Dirty = first.Result.Opportunities[0].Cycle.Pools
	next.Version.TxIndex = 1
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	if w.Rebase(&first) || w.Current(first) {
		t.Fatal("stale queued candidate survived changed pool")
	}
	releaseSearch(first.Job)
	close(q.finish)
}

// Frames that change another pool neither cancel the active path nor make a
// later job repeat it: the first job's quote is delivered at the newest frame
// and its pool is acknowledged when the job ends. The pending job used to
// carry every unacknowledged pool and republished this route from the exact
// cache.
func TestIncrementalUnrelatedDirtyFramesKeepCurrentPathWithoutRepeatingIt(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, false)
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.entered)
	latest := job
	for index := uint64(1); index <= 6; index++ {
		latest.Version.TxIndex = index
		latest.Dirty = []domain.PoolKey{job.Dirty[1]}
		if err := w.Submit(latest); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-q.canceled:
		t.Fatal("unrelated dirty path canceled active path")
	default:
	}
	close(q.finish)
	delivered := 0
	for count := 0; count < 12; count++ {
		e := nextSearch(t, w)
		origin := e.Job.Version
		valid := w.Rebase(&e)
		if valid {
			w.Accept(e)
		}
		if valid && e.Partial && slices.Contains(e.Result.Opportunities[0].Cycle.Pools, q.block) {
			if origin != job.Version || e.Job.Version != latest.Version {
				t.Fatal("unchanged route was searched again by the pending batch", origin)
			}
			delivered++
		}
		final := origin == latest.Version && !e.Partial
		releaseSearch(e.Job)
		if final {
			if delivered == 0 {
				t.Fatal("active path's quote was not delivered at the newest frame")
			}
			return
		}
	}
	t.Fatal("latest dirty batch did not finish")
}

func TestIncrementalContextBoundaryCancelsEvenAnEmptyFrame(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, false)
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.entered)
	next := job
	next.Version.BlockNumber++
	next.Dirty = nil
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.canceled)
	for count := 0; count < 8; count++ {
		e := nextSearch(t, w)
		valid := w.Rebase(&e)
		if valid && e.Job.Version != next.Version {
			t.Fatal("old context rebased across block boundary")
		}
		if valid {
			w.Accept(e)
		}
		final := valid && !e.Partial
		releaseSearch(e.Job)
		if final {
			return
		}
	}
	t.Fatal("context boundary lost accumulated dirty work")
}

func TestIncrementalConfirmationRecheckDoesNotInvalidatePoolInputs(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, false)
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.entered)
	next := job
	next.Dirty = nil
	next.Recheck = []domain.PoolKey{q.block}
	next.Version.TxIndex++
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	select {
	case <-q.canceled:
		t.Fatal("confirmation recheck was mistaken for changed pool data")
	default:
	}
	close(q.finish)
	reused := 0
	for count := 0; count < 12; count++ {
		event := nextSearch(t, w)
		if !w.Rebase(&event) || !w.Accept(event) {
			t.Fatal("unchanged confirmation recheck became stale")
		}
		reused += event.Result.ExactReused
		releaseSearch(event.Job)
		if !event.Partial && !w.PendingAfter(event) {
			if reused == 0 {
				t.Fatal("confirmation recheck repeated exact sizing")
			}
			return
		}
	}
	t.Fatal("confirmation recheck never settled")
}

func TestIncrementalHeartbeatResumesDirtyAfterSourceRejectedCompletedBatch(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, false)
	close(q.finish)
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	for {
		event := nextSearch(t, w)
		// The writer cannot accept while a newer input is unapplied. It hands
		// back each candidate whose pools are unchanged (SearchScheduler.Process).
		w.Recheck(event)
		releaseSearch(event.Job)
		if !event.Partial {
			break
		}
	}
	deadline := time.Now().Add(time.Second)
	for {
		w.mu.Lock()
		idle := len(w.runs[domain.SourceFast].alive) == 0
		w.mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("completed batch did not exit")
		}
		time.Sleep(time.Millisecond)
	}
	quoted := q.firstCalls.Load()
	next := job
	next.Dirty = nil
	next.Version.TxIndex++
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	for count := 0; count < 5; count++ {
		event := nextSearch(t, w)
		if !w.Rebase(&event) || !w.Accept(event) {
			t.Fatal("candidates the writer dropped were not recoverable")
		}
		final := !event.Partial
		releaseSearch(event.Job)
		if final {
			if q.firstCalls.Load() != quoted {
				t.Fatal("recovery discarded valid exact cache")
			}
			return
		}
	}
	t.Fatal("heartbeat left the dropped candidates' routes idle")
}
