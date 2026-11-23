package remote

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestShadowIntentPreemptsPaperSamplingWithItsQuoteState(t *testing.T) {
	r, event := reactiveFixture(t)
	p := &executionPaper{jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{IntervalMS: 1000, SamplingWindowMS: 1000, WrappedNative: event.Result.Opportunities[0].Cycle.Start.Hex()}}
	r.executionPaper, r.block = p, rpc.ExecutionBlock{Ref: r.store.Block()}
	if err := r.offerExecutionPaper(event); err != nil || p.window == nil {
		t.Fatal("paper sampling window not opened", err)
	}
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	<-r.reactive.jobs
	job := r.reactive.takeJob(r.reactive.primary())
	if record := r.reactive.executeJob(t.Context(), job, r.reactive.primary()); record["status"] != "shadow_admitted" || !r.reactive.verificationPending() {
		t.Fatal("shadow intent was not handed to verification", record["status"])
	}
	r.captureExecutionPaper(event.Job.Version, time.Now())
	if p.window != nil || !p.busy.Load() || len(p.jobs) != 1 || r.reactive.verificationPending() {
		t.Fatal("verification did not preempt the sampling window")
	}
	got := <-p.jobs
	if got.intent == nil || got.intent.ID != job.intent.ID || got.event.Job.Version != event.Job.Version || got.unavailable != "" {
		t.Fatal("verification lost the intent or its quote state", got.unavailable)
	}
	if got.opportunity.Input.Cmp(job.intent.Route.Amount) != 0 {
		t.Fatal("verification economics use a different principal than the intent")
	}
}
