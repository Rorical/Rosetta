package arb

import (
	"context"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestWorkerObservesCanceledAndSupersededWork(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, v2CurveQuoter())
	ctx, cancel := context.WithCancel(t.Context())
	worker, err := NewWorker(ctx, engine, rankedOptions(first.Token0, 100000))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { cancel(); <-worker.Done() }()
	observations := make(chan SearchObservation, 8)
	job := SearchJob{View: newFakeView().add(first).add(second), Dirty: []domain.PoolKey{first.Key}, GraphVersion: 1,
		Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: 1}, ReceivedAt: time.Now(),
		Observe: func(observation SearchObservation) { observations <- observation }}
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	blocked := job
	blocked.Stages.Rank = time.Hour // A dependency retry can carry prior-stage fields.
	blocked.Prepare = func(ctx context.Context, view View, _ []domain.PoolKey) (View, []QuoteIssue, error) {
		close(entered)
		<-release
		return view, nil, ctx.Err()
	}
	if err := worker.Submit(blocked); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("preparation did not start")
	}
	for epoch := uint64(2); epoch <= 3; epoch++ {
		job.Version.Epoch = epoch
		if err := worker.Submit(job); err != nil {
			t.Fatal(err)
		}
	}
	worker.Invalidate(domain.SourceStandard)
	job.Version.Epoch = 4
	if err := worker.Submit(job); err != nil {
		t.Fatal(err)
	}
	close(release)
	event := workerResult(t, worker, job.Version)
	if event.Err != nil || event.Job.Observe != nil || event.Job.ReceivedAt != job.ReceivedAt {
		t.Fatalf("result retained observer or lost trigger: %+v", event)
	}
	counts := make(map[SearchOutcome]int)
	for range 4 {
		select {
		case observation := <-observations:
			if observation.QueueWait < 0 || observation.Evaluation < 0 {
				t.Fatal("invalid measured duration")
			}
			if observation.Outcome == SearchCanceled && (observation.Stages.Prepare <= 0 || observation.Stages.Rank != 0) {
				t.Fatal("canceled retry lost current timing or retained a prior rank duration")
			}
			counts[observation.Outcome]++
		case <-time.After(5 * time.Second):
			t.Fatal("canceled or superseded work was invisible")
		}
	}
	if counts[SearchCanceled] != 1 || counts[SearchSuperseded] != 2 || counts[SearchCompleted] != 1 {
		t.Fatalf("outcomes: %+v", counts)
	}
}
