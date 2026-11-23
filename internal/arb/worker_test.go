package arb

import (
	"context"
	"math/big"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

type blockingCurveQuoter struct {
	once    sync.Once
	entered chan struct{}
}

func (q *blockingCurveQuoter) QuoteExactIn(ctx context.Context, p pool.State, _ pool.DeepState, token domain.Address, amount *big.Int) (*big.Int, error) {
	blocked := false
	q.once.Do(func() { close(q.entered); blocked = true })
	if blocked {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	in, out, _ := traversalReserves(p, token)
	return math.QuoteV2ExactIn(in, out, p.Fee, amount)
}

func workerResult(t *testing.T, worker *Worker, version domain.StateVersion) SearchEvent {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-worker.Results():
			if event.Job.Version == version {
				return event
			}
			releaseSearch(event.Job)
		case <-timer.C:
			t.Fatal("latest job did not finish")
			return SearchEvent{}
		}
	}
}

func TestWorkerSupersedesJobWithoutLosingEarlierDirtyPools(t *testing.T) {
	states := []pool.State{
		testV2State(0x10, 1, 2, 1_000_000, 2_000_000), testV2State(0x20, 2, 1, 1_000_000, 2_000_000),
		testV2State(0x30, 3, 4, 1_000_000, 2_000_000), testV2State(0x40, 4, 3, 1_000_000, 2_000_000),
	}
	quoter := &blockingCurveQuoter{entered: make(chan struct{})}
	engine := mustEngine(t, mustTable(t, 2, states...), 1000, 1, quoter)
	view := newFakeView()
	for _, p := range states {
		view.add(p)
	}
	options := rankedOptions(states[0].Token0, 100000)
	options.Bounds = func(domain.Address) (InputBounds, bool) {
		return InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}, true
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker, err := NewWorker(ctx, engine, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	first := SearchJob{View: view, GraphVersion: 1, Dirty: []domain.PoolKey{states[0].Key}, Version: domain.StateVersion{Source: domain.SourceStandardPrefix, Epoch: 1}}
	if err := worker.Submit(first); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quoter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first quote did not start")
	}
	latest := first
	latest.Dirty = []domain.PoolKey{states[2].Key}
	latest.Version = domain.StateVersion{Source: domain.SourceStandard, Epoch: 2}
	if err := worker.Submit(latest); err != nil {
		t.Fatal(err)
	}
	event := workerResult(t, worker, latest.Version)
	if event.Err != nil || len(event.Job.Dirty) != 2 || len(event.Result.Opportunities) != 4 {
		t.Fatalf("superseded job lost dirty pools: %+v", event)
	}
	if worker.Accept(SearchEvent{Job: first}) || !worker.Accept(event) {
		t.Fatal("acknowledgement accepted old work or rejected current work")
	}
}

func TestWorkerInvalidationCancelsFastAndLeavesStandardSlotUsable(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	quoter := &blockingCurveQuoter{entered: make(chan struct{})}
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, quoter)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	job := SearchJob{View: newFakeView().add(first).add(second), Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-quoter.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Fast quote did not start")
	}
	worker.Invalidate(domain.SourceFast)
	job.Version = domain.StateVersion{Source: domain.SourceStandard, Epoch: 2}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	event := workerResult(t, worker, job.Version)
	if event.Err != nil || len(event.Result.Opportunities) != 1 || !worker.Accept(event) {
		t.Fatalf("Fast invalidation disrupted standard search: %+v", event)
	}
}

// A caller that quotes beside the worker keeps the cores it reserves; the
// worker always keeps one for itself.
func TestWorkerLeavesReservedCPUs(t *testing.T) {
	procs := runtime.GOMAXPROCS(0)
	for _, test := range []struct{ reserved, want int }{{0, procs}, {1, max(1, procs-1)}, {procs + 3, 1}, {-2, procs}} {
		opts := RankedOptions{Bounds: func(domain.Address) (InputBounds, bool) { return InputBounds{}, false }, SwapFee: func(pool.State, domain.Address) (uint32, error) { return 0, nil }, ReservedCPUs: test.reserved}
		ctx, cancel := context.WithCancel(t.Context())
		w, err := NewWorker(ctx, &Engine{}, opts)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if w.cpus.capacity != test.want {
			t.Fatalf("reserved %d of %d: search capacity %d, want %d", test.reserved, procs, w.cpus.capacity, test.want)
		}
	}
}
