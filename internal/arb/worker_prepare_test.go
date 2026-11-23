package arb

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestWorkerCancelsPreparationAndPreservesAccumulatedDirtyPools(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, v2CurveQuoter())
	view := newFakeView().add(first).add(second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	entered := make(chan struct{})
	job := SearchJob{View: view, Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1},
		Prepare: func(ctx context.Context, raw View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
			close(entered)
			<-ctx.Done()
			return raw, nil, ctx.Err()
		}}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dependency preparation did not start")
	}
	latest := job
	latest.Version.Epoch++
	latest.Dirty = []domain.PoolKey{second.Key}
	failure := errors.New("dependency unavailable")
	latest.Prepare = func(_ context.Context, raw View, dirty []domain.PoolKey) (View, []QuoteIssue, error) {
		if !slices.Equal(dirty, []domain.PoolKey{first.Key, second.Key}) {
			t.Error("superseding preparation lost accumulated dirty pools")
		}
		return raw, []QuoteIssue{{Pool: first.Key, Err: failure}}, nil
	}
	if err := worker.Submit(latest); err != nil {
		t.Fatal(err)
	}
	event := workerResult(t, worker, latest.Version)
	if event.Err != nil || len(event.Result.Opportunities) == 0 || len(event.Result.Issues) != 1 || !errors.Is(event.Result.Issues[0].Err, failure) || event.Job.Prepare != nil || !worker.Accept(event) {
		t.Fatalf("prepared result or dependency ownership lost: %+v", event)
	}
	if worker.Current(event) || worker.Accept(event) {
		t.Fatal("an acknowledged result remained available for duplicate output or retry")
	}
}

func TestPreparationFailureCannotQuoteTheUnpreparedView(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	quoter := v2CurveQuoter()
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, quoter)
	view := newFakeView().add(first).add(second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	failure := errors.New("snapshot read failed")
	job := SearchJob{View: view, Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1},
		Prepare: func(context.Context, View, []domain.PoolKey) (View, []QuoteIssue, error) { return nil, nil, failure }}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	event := workerResult(t, worker, job.Version)
	if !errors.Is(event.Err, failure) || len(quoter.calls) != 0 || worker.Accept(event) {
		t.Fatal("dependency preparation failure fell back to stale pricing")
	}
}
