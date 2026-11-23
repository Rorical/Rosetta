package arb

import (
	"context"
	"math/big"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type delayedExitQuote struct {
	entered, exit chan struct{}
	once          sync.Once
}

func (q *delayedExitQuote) QuoteExactIn(ctx context.Context, _ pool.State, _ pool.DeepState, _ domain.Address, _ *big.Int) (*big.Int, error) {
	q.once.Do(func() { close(q.entered) })
	<-q.exit // Simulate a reader that has not acknowledged cancellation yet.
	return nil, ctx.Err()
}

func TestPipelineJoinsCanceledReaderBeforeReleasingFrame(t *testing.T) {
	a, b := testV2State(1, 1, 2, 1000000, 2000000), testV2State(2, 2, 1, 1000000, 2000000)
	q := &delayedExitQuote{entered: make(chan struct{}), exit: make(chan struct{})}
	engine := mustEngine(t, explicitTable(t, a.Token0, []domain.PoolKey{a.Key, b.Key}), 1000, 1, q)
	opts := rankedOptions(a.Token0, 100000)
	opts.StreamInitial = true
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker, err := NewWorker(ctx, engine, opts)
	if err != nil {
		t.Fatal(err)
	}
	var closes atomic.Int64
	if err := worker.Submit(SearchJob{Incremental: true, View: newFakeView().add(a).add(b), Dirty: []domain.PoolKey{a.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}, Release: func() { closes.Add(1) }}); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.entered)
	worker.Invalidate(domain.SourceFast)
	cancel()
	if closes.Load() != 0 {
		t.Fatal("released executing reader on cancellation")
	}
	close(q.exit)
	<-worker.Done()
	if closes.Load() != 1 {
		t.Fatal("reader release lost or duplicated", closes.Load())
	}
}
