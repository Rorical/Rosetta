package arb

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestWorkerLoadsOnlyRankedDependenciesAndDeliversPreparedView(t *testing.T) {
	first := testV2State(10, 1, 2, 1_000_000, 2_000_000)
	lower := testV2State(20, 2, 1, 1_000_000, 1_500_000)
	winner := testV2State(30, 2, 1, 1_000_000, 2_000_000)
	table := explicitTable(t, first.Token0, []domain.PoolKey{first.Key, lower.Key}, []domain.PoolKey{first.Key, winner.Key})
	quoter := v2CurveQuoter()
	engine := mustEngine(t, table, 1000, 1, quoter)
	view := newFakeView().add(first).add(lower).add(winner)
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100_000))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	prepared := newFakeView().add(first).add(winner)
	invoked := 0
	job := SearchJob{View: view, Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1},
		PrepareExact: func(_ context.Context, raw View, keys []domain.PoolKey) (View, []QuoteIssue, error) {
			invoked++
			if raw != view || len(quoter.calls) != 0 || !slices.Equal(keys, []domain.PoolKey{first.Key, winner.Key}) {
				t.Error("exact preparation preceded selection or included discarded path")
			}
			return prepared, nil, nil
		}}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	event := workerResult(t, worker, job.Version)
	if event.Err != nil || invoked != 1 || event.Job.View != prepared || event.Job.PrepareExact != nil || event.Result.Selected != 1 || len(event.Result.Opportunities) != 1 {
		t.Fatal("staged selection did not retain exact snapshot", event.Err)
	}
}

func TestExactPreparationFailureNeverUsesUnpreparedState(t *testing.T) {
	first := testV2State(10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(20, 2, 1, 1_000_000, 2_000_000)
	quoter := v2CurveQuoter()
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, quoter)
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100_000))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	failure := errors.New("selected dependency unavailable")
	job := SearchJob{View: newFakeView().add(first).add(second), Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1},
		PrepareExact: func(context.Context, View, []domain.PoolKey) (View, []QuoteIssue, error) { return nil, nil, failure }}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	event := workerResult(t, worker, job.Version)
	if !errors.Is(event.Err, failure) || len(quoter.calls) != 0 || worker.Accept(event) {
		t.Fatal("failed exact preparation used stale state")
	}
}

func TestExactPreparationCancellationWaitsForReaderExit(t *testing.T) {
	first := testV2State(10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(20, 2, 1, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, v2CurveQuoter())
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100_000))
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	entered, canceled, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-exit:
		default:
			close(exit)
		}
		<-worker.Done()
	})
	var releases atomic.Int32
	job := SearchJob{View: newFakeView().add(first).add(second), Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1}, Release: func() { releases.Add(1) },
		PrepareExact: func(ctx context.Context, view View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-exit
			return nil, nil, ctx.Err()
		}}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("exact preparation did not start")
	}
	latest := job
	latest.Version.Epoch++
	latest.PrepareExact = nil
	latest.Release = nil
	if err := worker.Submit(latest); err != nil {
		t.Fatal(err)
	}
	select {
	case <-canceled:
	case <-time.After(5 * time.Second):
		t.Fatal("new version did not cancel selected dependency read")
	}
	if releases.Load() != 0 {
		t.Fatal("cancellation released snapshot while getter was still running")
	}
	close(exit)
	event := workerResult(t, worker, latest.Version)
	if event.Err != nil || len(event.Result.Opportunities) != 1 || releases.Load() != 1 {
		t.Fatal("latest version failed after actual reader exit", event.Err)
	}
}
