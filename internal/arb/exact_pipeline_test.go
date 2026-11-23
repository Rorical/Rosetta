package arb

import (
	"context"
	"math/big"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// The long route ranks only after the short route's sizing is over, however
// many cores are idle. The earlier pipeline discovered it while the short
// route was still being sized.
func TestLongRoutesRankAfterShortRoutesAreSized(t *testing.T) {
	pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000)}
	long := []domain.PoolKey{}
	for i := byte(0); i < 6; i++ {
		from, to := byte(10+i), byte(11+i)
		if i == 0 {
			from = 1
		}
		if i == 5 {
			to = 1
		}
		p := testV2State(10+i, from, to, 1000000, 2000000)
		pools = append(pools, p)
		long = append(long, p.Key)
	}
	q := &heldAmountSearch{inner: v2CurveQuoter(), key: pools[0].Key, entered: make(chan struct{}), release: make(chan struct{})}
	engine := mustEngine(t, explicitTable(t, pools[0].Token0, []domain.PoolKey{pools[0].Key, pools[1].Key}, long), 1000, 2, q)
	opts := rankedOptions(pools[0].Token0, 100000)
	opts.StreamInitial = true
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	worker.cpus = newSearchCPUsOf(2)
	t.Cleanup(func() { cancel(); <-worker.Done() })
	view := newFakeView()
	for _, p := range pools {
		view.add(p)
	}
	discovered := make(chan struct{})
	var once sync.Once
	prepare := func(_ context.Context, v View, keys []domain.PoolKey) (View, []QuoteIssue, error) {
		for _, key := range keys {
			if key == long[0] {
				select {
				case <-q.release:
				default:
					t.Error("long route ranked while the short route was being sized")
				}
				once.Do(func() { close(discovered) })
			}
		}
		return v, nil, nil
	}
	if err := worker.Submit(SearchJob{Incremental: true, View: view, Dirty: []domain.PoolKey{pools[0].Key, long[0]}, PrepareExact: prepare, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}); err != nil {
		t.Fatal(err)
	}
	// Drain publication so the only barrier is the held protocol calculation.
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for e := range worker.Results() {
			worker.Accept(e)
			releaseSearch(e.Job)
		}
	}()
	waitSignal(t, q.entered)
	close(q.release)
	waitSignal(t, discovered)
	cancel()
	<-worker.Done()
	<-drained
}

func TestParallelInitialProbeDoesNotWaitForBlockedNeighbour(t *testing.T) {
	worker, job, q, _ := incrementalFixture(t, false)
	worker.cpus = newSearchCPUsOf(2)
	worker.opts.StreamInitial = true
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.entered)
	event := nextSearch(t, worker)
	if event.Stage != QuoteInitial {
		t.Fatal("unblocked probe did not publish", event.Stage)
	}
	releaseSearch(event.Job)
}

type sizingTurns struct {
	inner                      Quoter
	keys                       map[domain.PoolKey]bool
	counts                     map[domain.PoolKey]int
	first                      domain.PoolKey
	firstCountWhenOtherResumes int
}

func (q *sizingTurns) QuoteExactIn(ctx context.Context, p pool.State, d pool.DeepState, a domain.Address, n *big.Int) (*big.Int, error) {
	if q.keys[p.Key] {
		if q.first == (domain.PoolKey{}) {
			q.first = p.Key
		}
		q.counts[p.Key]++
		if p.Key != q.first && q.counts[p.Key] == 2 {
			q.firstCountWhenOtherResumes = q.counts[q.first]
		}
	}
	return q.inner.QuoteExactIn(ctx, p, d, a, n)
}

func TestSingleCPUResumesOtherAmountSearchBeforeFinishingFirst(t *testing.T) {
	pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000), testV2State(3, 1, 3, 1000000, 2000000), testV2State(4, 3, 1, 1000000, 2000000)}
	q := &sizingTurns{inner: v2CurveQuoter(), keys: map[domain.PoolKey]bool{pools[0].Key: true, pools[2].Key: true}, counts: map[domain.PoolKey]int{}}
	engine := mustEngine(t, explicitTable(t, pools[0].Token0, []domain.PoolKey{pools[0].Key, pools[1].Key}, []domain.PoolKey{pools[2].Key, pools[3].Key}), 1000, 2, q)
	opts := rankedOptions(pools[0].Token0, 100000)
	opts.StreamInitial = true
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	worker.cpus = newSearchCPUsOf(1)
	t.Cleanup(func() { cancel(); <-worker.Done() })
	view := newFakeView()
	var dirty []domain.PoolKey
	for _, p := range pools {
		view.add(p)
		dirty = append(dirty, p.Key)
	}
	if err := worker.Submit(SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}); err != nil {
		t.Fatal(err)
	}
	optimized := 0
	for {
		event := nextSearch(t, worker)
		worker.Accept(event)
		releaseSearch(event.Job)
		if event.Stage == QuoteOptimized {
			optimized++
		}
		if !event.Partial {
			break
		}
	}
	if q.firstCountWhenOtherResumes == 0 || q.firstCountWhenOtherResumes > 33 || optimized != 2 {
		t.Fatal("sizing monopolized CPU or lost completion", q.firstCountWhenOtherResumes, optimized)
	}
}
