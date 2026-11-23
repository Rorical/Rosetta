package arb

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestWorkerTransfersReaderOwnershipThroughCancellationAndDelivery(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, v2CurveQuoter())
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); <-worker.Done() }()
	var released [7]atomic.Uint32
	job := func(epoch uint64) SearchJob {
		return SearchJob{View: newFakeView().add(first).add(second), Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1,
			Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: epoch}, Release: func() { released[epoch].Add(1) }}
	}
	entered, exit := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-exit:
		default:
			close(exit)
		}
	}()
	active := job(1)
	active.Prepare = func(ctx context.Context, view View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
		close(entered)
		<-exit
		return view, nil, ctx.Err()
	}
	if err := worker.Submit(active); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not start")
	}
	if err := worker.Submit(job(2)); err != nil {
		t.Fatal(err)
	}
	if err := worker.Submit(job(3)); err != nil {
		t.Fatal(err)
	}
	worker.Invalidate(domain.SourceStandard)
	if released[1].Load() != 0 || released[2].Load() != 1 || released[3].Load() != 1 {
		t.Fatal("cancellation released an active reader or retained a pending reader")
	}
	if err := worker.Submit(job(4)); err != nil {
		t.Fatal(err)
	}
	close(exit)
	event := workerResult(t, worker, job(4).Version)
	if released[1].Load() != 1 || released[4].Load() != 0 || event.Job.Release == nil || !worker.Accept(event) {
		t.Fatal("reader was lost before delivery or after canceled evaluation")
	}
	event.Job.Release()
	empty := job(5)
	empty.Dirty = nil
	if err := worker.Submit(empty); err != nil || released[5].Load() != 1 {
		t.Fatal("empty job retained its reader")
	}
	// An unread delivered result remains owned by the result channel at stop.
	if err := worker.Submit(job(6)); err != nil {
		t.Fatal(err)
	}
	last := workerResult(t, worker, job(6).Version)
	worker.results <- last
	cancel()
	<-worker.Done()
	if released[4].Load() != 1 || released[6].Load() != 1 {
		t.Fatal("result consumer or shutdown lost ownership")
	}
	rejected := job(6)
	if err := worker.Submit(rejected); err == nil || released[6].Load() != 1 {
		t.Fatal("stopped worker accepted ownership")
	}
	rejected.Release() // failed Submit leaves ownership with the caller
}
