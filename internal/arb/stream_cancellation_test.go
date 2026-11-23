package arb

import (
	"context"
	"math/big"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type heldAmountSearch struct {
	inner            Quoter
	key              domain.PoolKey
	calls            atomic.Int64
	entered, release chan struct{}
}

func (q *heldAmountSearch) QuoteExactIn(ctx context.Context, p pool.State, deep pool.DeepState, token domain.Address, amount *big.Int) (*big.Int, error) {
	if p.Key == q.key && q.calls.Add(1) == 2 {
		close(q.entered)
		select {
		case <-q.release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return q.inner.QuoteExactIn(ctx, p, deep, token, amount)
}

func TestEarlyRefinementRetainsUnchangedPathAndCancelsChangedPath(t *testing.T) {
	for _, changed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unrelated", true: "related"}[changed], func(t *testing.T) {
			pools := []pool.State{testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000), testV2State(3, 3, 4, 1000000, 2000000)}
			quote := &heldAmountSearch{inner: v2CurveQuoter(), key: pools[0].Key, entered: make(chan struct{}), release: make(chan struct{})}
			engine := mustEngine(t, explicitTable(t, pools[0].Token0, []domain.PoolKey{pools[0].Key, pools[1].Key}), 1000, 1, quote)
			opts := rankedOptions(pools[0].Token0, 100000)
			opts.StreamInitial = true
			ctx, cancel := context.WithCancel(t.Context())
			worker, err := NewWorker(ctx, engine, opts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { cancel(); <-worker.Done() })
			view := newFakeView()
			for _, p := range pools {
				view.add(p)
			}
			job := SearchJob{Incremental: true, View: view, Dirty: []domain.PoolKey{pools[0].Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 1}}
			if err := worker.Submit(job); err != nil {
				t.Fatal(err)
			}
			initial := nextSearch(t, worker)
			worker.Accept(initial)
			releaseSearch(initial.Job)
			waitSignal(t, quote.entered)
			next := job
			next.Version.TxIndex = 1
			next.Dirty = []domain.PoolKey{pools[2].Key}
			if changed {
				next.Dirty = []domain.PoolKey{pools[0].Key}
				fresh := newFakeView()
				for _, p := range pools {
					if p.Key == pools[0].Key {
						p.Reserve1 = big.NewInt(3000000)
					}
					fresh.add(p)
				}
				next.View = fresh
			}
			if err := worker.Submit(next); err != nil {
				t.Fatal(err)
			}
			close(quote.release)
			// The newer frame's search no longer waits for the held one, so
			// the finals arrive in either order.
			oldOptimized, finals := false, 0
			for finals < 2 {
				event := nextSearch(t, worker)
				if !event.Partial {
					finals++
				}
				if event.Stage == QuoteOptimized && event.OriginalJob().Version.TxIndex == 0 {
					if !worker.Rebase(&event) || !worker.Accept(event) {
						t.Fatal("unchanged completed route lost its proof")
					}
					oldOptimized = true
				} else {
					worker.Rebase(&event)
					worker.Accept(event)
				}
				releaseSearch(event.Job)
			}
			if oldOptimized == changed {
				t.Fatal("incorrect dependency-scoped sizing cancellation", changed, oldOptimized)
			}
		})
	}
}
