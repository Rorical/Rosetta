package arb

import (
	"context"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestFastSearchDoesNotWaitForStandardReaderToExit(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, v2CurveQuoter())
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	entered, exit := make(chan struct{}), make(chan struct{})
	defer func() { close(exit); cancel(); <-worker.Done() }()
	job := SearchJob{View: newFakeView().add(first).add(second), Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1,
		Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1},
		Prepare: func(ctx context.Context, view View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
			close(entered)
			<-exit
			return view, nil, ctx.Err()
		}}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("standard preparation did not start")
	}
	job.Version.Source, job.Prepare = domain.SourceFast, nil
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-worker.Results():
		if event.Job.Version != job.Version || event.Err != nil || !worker.Accept(event) {
			t.Fatalf("unexpected Fast result: %+v", event)
		}
		releaseSearch(event.Job)
	case <-time.After(time.Second):
		t.Fatal("Fast search waited for the blocked standard reader")
	}
}
