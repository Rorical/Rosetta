package pipeline

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type controlledTask struct {
	run    func(context.Context)
	closed atomic.Int32
}

func (t *controlledTask) Run(ctx context.Context) (*execution.ReadyPlan, []string, error) {
	if t.run != nil {
		t.run(ctx)
	}
	return nil, nil, ctx.Err()
}
func (t *controlledTask) Close() { t.closed.Add(1) }
func await[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case value := <-ch:
		return value
	case <-time.After(5 * time.Second):
		t.Fatal("worker did not reach expected boundary")
		var zero T
		return zero
	}
}
func confirmationJob(task ConfirmationTask, epoch uint64) ConfirmationJob {
	return ConfirmationJob{Task: task, Identity: ConfirmationIdentity{Version: domain.StateVersion{Source: domain.SourceStandard, Epoch: epoch}, Graph: 1}}
}

func TestConfirmationSupersessionWaitsForActualExitBeforeClosingSnapshot(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	worker := NewConfirmationWorker(ctx, time.Minute)
	entered, canceled, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	active := &controlledTask{run: func(ctx context.Context) { close(entered); <-ctx.Done(); close(canceled); <-exit }}
	t.Cleanup(func() {
		cancel()
		select {
		case <-exit:
		default:
			close(exit)
		}
		<-worker.Done()
	})
	if err := worker.Submit(confirmationJob(active, 1)); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	pending := &controlledTask{}
	if err := worker.Submit(confirmationJob(pending, 2)); err != nil {
		t.Fatal(err)
	}
	await(t, canceled)
	latest := &controlledTask{}
	if err := worker.Submit(confirmationJob(latest, 3)); err != nil {
		t.Fatal(err)
	}
	if active.closed.Load() != 0 || pending.closed.Load() != 1 {
		t.Fatal("cancellation released active state or retained superseded pending state")
	}
	close(exit)
	event := await(t, worker.Results())
	if event.Identity.Version.Epoch != 3 || !worker.Current(event) || active.closed.Load() != 1 || latest.closed.Load() != 1 {
		t.Fatal("stale task delivered or task lease not closed exactly once")
	}
	worker.Invalidate()
	if worker.Current(event) {
		t.Fatal("invalidation kept old ticket current")
	}
	cancel()
	await(t, worker.Done())
	rejected := &controlledTask{}
	if err := worker.Submit(confirmationJob(rejected, 4)); err == nil || rejected.closed.Load() != 0 {
		t.Fatal("rejected submission transferred ownership")
	}
	rejected.Close()
}

func TestConfirmationLanesProgressIndependentlyAndShutdownJoins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fast, standard := NewConfirmationWorker(ctx, time.Minute), NewConfirmationWorker(ctx, time.Minute)
	entered, canceled, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	task := &controlledTask{run: func(ctx context.Context) { close(entered); <-ctx.Done(); close(canceled); <-exit }}
	t.Cleanup(func() {
		cancel()
		select {
		case <-exit:
		default:
			close(exit)
		}
		<-fast.Done()
		<-standard.Done()
	})
	job := confirmationJob(task, 1)
	job.Identity.Version.Source = domain.SourceFast
	if err := fast.Submit(job); err != nil {
		t.Fatal(err)
	}
	await(t, entered)
	if err := standard.Submit(confirmationJob(&controlledTask{}, 2)); err != nil {
		t.Fatal(err)
	}
	event := await(t, standard.Results())
	fast.Invalidate()
	await(t, canceled)
	if !standard.Current(event) || task.closed.Load() != 0 {
		t.Fatal("Fast cancellation affected canonical lane or released active state")
	}
	cancel()
	select {
	case <-fast.Done():
		t.Fatal("shutdown did not join active task")
	default:
	}
	close(exit)
	await(t, fast.Done())
	await(t, standard.Done())
	if task.closed.Load() != 1 {
		t.Fatal("shutdown leaked snapshot")
	}
}
