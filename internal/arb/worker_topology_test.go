package arb

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestWorkerSwitchesTopologyWhileOldEvaluationIsActive(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	newPool := testV2State(0x30, 2, 1, 1_000_000, 4_000_000)
	blocked := &blockingCurveQuoter{entered: make(chan struct{})}
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, blocked)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-worker.Done() })
	job := SearchJob{View: newFakeView().add(first).add(second).add(newPool), Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1}}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("old topology was not evaluating")
	}
	next := mustEngine(t, mustTable(t, 2, first, second, newPool), 1000, 1, v2CurveQuoter())
	if err := worker.ReplaceEngine(next, 2); err != nil {
		t.Fatal(err)
	}
	if err := worker.Submit(job); err == nil {
		t.Fatal("old graph request was accepted after the switch")
	}
	job.GraphVersion = 2
	job.Dirty = []domain.PoolKey{newPool.Key}
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	event := workerResult(t, worker, job.Version)
	if event.Err != nil || event.Job.GraphVersion != 2 || len(event.Result.Opportunities) != 1 || !slices.Contains(event.Result.Opportunities[0].Cycle.Pools, newPool.Key) || !worker.Accept(event) {
		t.Fatalf("new job used the wrong topology: %+v", event)
	}
	if err := worker.ReplaceEngine(engine, 2); err == nil {
		t.Fatal("topology version was reused")
	}
}
