package pipeline

import (
	"context"
	"testing"
	"time"
)

func TestConfirmationOffersKeepEarlierCandidateAndRespectPrefixCap(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	w := NewConfirmationWorker(ctx, time.Minute)
	entered, exit, canceled := make(chan struct{}), make(chan struct{}), make(chan struct{})
	first := &controlledTask{run: func(ctx context.Context) {
		close(entered)
		select {
		case <-exit:
		case <-ctx.Done():
			close(canceled)
		}
	}}
	t.Cleanup(func() { cancel(); <-w.Done() })
	if ok, err := w.Offer(confirmationJob(first, 1), 2); err != nil || !ok {
		t.Fatal(err)
	}
	await(t, entered)
	second := &controlledTask{}
	if ok, err := w.Offer(confirmationJob(second, 1), 2); err != nil || !ok {
		t.Fatal(err)
	}
	select {
	case <-canceled:
		t.Fatal("later candidate canceled earlier confirmation")
	default:
	}
	excess := &controlledTask{}
	if ok, err := w.Offer(confirmationJob(excess, 1), 2); err != nil || ok || excess.closed.Load() != 0 {
		t.Fatal("cap transferred rejected task ownership", ok, err)
	}
	excess.Close()
	close(exit)
	for index := range 2 {
		event := await(t, w.Results())
		if w.PendingAfter(event) != (index == 0) {
			t.Fatal("queued confirmation disappeared from pending accounting", index)
		}
		if !w.Current(event) || event.Err != nil {
			t.Fatal("same-prefix FIFO lost current result", event)
		}
	}
	if first.closed.Load() != 1 || second.closed.Load() != 1 {
		t.Fatal("confirmation snapshots leaked")
	}
	if ok, err := w.Offer(confirmationJob(&controlledTask{}, 2), 2); err != nil || !ok {
		t.Fatal("new prefix did not reset cap", err)
	}
	await(t, w.Results())
}

func TestConfirmationOfferInvalidationJoinsReaderAndClosesQueuedTasks(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	w := NewConfirmationWorker(ctx, time.Minute)
	entered, canceled, exit := make(chan struct{}), make(chan struct{}), make(chan struct{})
	first := &controlledTask{run: func(ctx context.Context) { close(entered); <-ctx.Done(); close(canceled); <-exit }}
	t.Cleanup(func() {
		cancel()
		select {
		case <-exit:
		default:
			close(exit)
		}
		<-w.Done()
	})
	w.Offer(confirmationJob(first, 1), 3)
	await(t, entered)
	second := &controlledTask{}
	w.Offer(confirmationJob(second, 1), 3)
	w.Invalidate()
	await(t, canceled)
	if first.closed.Load() != 0 || second.closed.Load() != 1 {
		t.Fatal("invalidation released executing reader or retained queued task")
	}
	close(exit)
	newest := &controlledTask{}
	w.Offer(confirmationJob(newest, 2), 3)
	e := await(t, w.Results())
	if !w.Current(e) || e.Identity.Version.Epoch != 2 || first.closed.Load() != 1 {
		t.Fatal("obsolete prefix survived invalidation")
	}
}
