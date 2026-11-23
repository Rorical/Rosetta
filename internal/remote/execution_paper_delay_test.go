package remote

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
)

func delayFixture() (*runtime, paperExecutionJob) {
	now := time.Now().Add(-300 * time.Millisecond)
	version := domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: 11}
	prefix := flashPrefix{Payload: "payload", Index: 1, Version: version, Base: feed.FlashblockBase{Number: 11, Timestamp: 1002, Parent: domain.Hash{10}}, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{1}}}}
	p := &executionPaper{jobs: make(chan paperExecutionJob, 3), policy: executionPaperPolicy{DelaysMS: []int64{200, 600}, MaxPrefixTransactions: 100}}
	p.busy.Store(true)
	job := paperExecutionJob{discovered: now, prefix: &prefix, event: arb.SearchEvent{Job: arb.SearchJob{Version: version, ReceivedAt: now}}}
	r := &runtime{executionPaper: p, flashPrefix: prefix}
	r.flash.active = true
	return r, job
}

func TestPaperDelayCapturesObservedPrefixAndExpiresMissing(t *testing.T) {
	r, job := delayFixture()
	p := r.executionPaper
	p.schedule(job)
	baseline := <-p.jobs
	// Processing an old received frame must not turn it into a delayed state.
	r.flashPrefix.Index++
	r.flashPrefix.Version.TxIndex++
	r.captureExecutionPaper(r.flashPrefix.Version, job.discovered.Add(150*time.Millisecond))
	if len(p.jobs) != 0 {
		t.Fatal("old frame relabelled as arrival")
	}
	observed := job.discovered.Add(250 * time.Millisecond)
	r.captureExecutionPaper(r.flashPrefix.Version, observed)
	arrival := <-p.jobs
	if arrival.delayMS != 200 || arrival.group != baseline.group || arrival.prefix == nil || arrival.observed != observed || arrival.event.Job.Version != baseline.event.Job.Version || arrival.executionVersion == baseline.executionVersion {
		t.Fatalf("incorrect arrival: %+v", arrival)
	}
	r.flashPrefix.Transactions[0].Hash = domain.Hash{99}
	if arrival.prefix.Transactions[0].Hash != (domain.Hash{1}) {
		t.Fatal("captured prefix is mutable")
	}
	p.expireAll(job.discovered.Add(1200 * time.Millisecond))
	missing := <-p.jobs
	result := p.evaluate(t.Context(), missing)
	if p.pending != nil || result.Status != "censored" || result.ActualDelayNS != 0 || p.backoff.Load() != 0 || result.ID == p.evaluate(t.Context(), paperExecutionJob{group: baseline.group, unavailable: "test"}).ID {
		t.Fatalf("missing observation misclassified: %+v", result)
	}
}

func TestPaperDelayCapturesNextBlockForCanonicalParentVerification(t *testing.T) {
	r, job := delayFixture()
	r.executionPaper.policy.PrefixSource = "canonical-trace"
	r.executionPaper.schedule(job)
	<-r.executionPaper.jobs
	r.flashPrefix.Payload = "next"
	r.flashPrefix.Base.Number++
	r.flashPrefix.Base.Timestamp += 2
	r.flashPrefix.Base.Parent = domain.Hash{11}
	r.flashPrefix.Version.BlockNumber++
	// Canonical reconciliation also changes the local quote epoch without
	// replacing the feed chain. Parent verification decides ancestry.
	r.flashPrefix.Version.Epoch++
	r.flashPrefix.Index = 0
	r.flashPrefix.Transactions = nil
	r.captureExecutionPaper(r.flashPrefix.Version, job.discovered.Add(250*time.Millisecond))
	later := <-r.executionPaper.jobs
	if later.censored || later.prefix == nil || later.prefix.Base.Number != 12 {
		t.Fatal("next block was discarded before checking its canonical parent", later.unavailable)
	}
	if later.quoteTimestamp != job.prefix.Base.Timestamp || later.event.Job.Version != job.event.Job.Version {
		t.Fatal("later state changed the original transaction context")
	}
}

func TestPaperCanonicalDelaySurvivesLocalQuoteRebase(t *testing.T) {
	r, job := delayFixture()
	r.executionPaper.policy.PrefixSource = "canonical-trace"
	r.executionPaper.schedule(job)
	<-r.executionPaper.jobs
	r.flashPrefix.Index++
	r.flashPrefix.Version.Epoch++
	r.captureExecutionPaper(r.flashPrefix.Version, job.discovered.Add(250*time.Millisecond))
	later := <-r.executionPaper.jobs
	if later.censored || later.prefix == nil || later.crossBlock {
		t.Fatal("local quote epoch invalidated an identical canonical prefix", later.unavailable)
	}
}

func TestPaperDelayRejectsBranchReplacementAndBlockCrossing(t *testing.T) {
	for _, change := range []func(*flashPrefix){
		func(p *flashPrefix) { p.Payload = "replacement" },
		func(p *flashPrefix) { p.Base.Number++ },
		func(p *flashPrefix) { p.Transactions = []feed.FlashTransaction{{Hash: domain.Hash{88}}} },
		func(p *flashPrefix) { p.Version.Epoch++ },
	} {
		r, job := delayFixture()
		r.executionPaper.schedule(job)
		<-r.executionPaper.jobs
		r.flashPrefix.Index++
		change(&r.flashPrefix)
		r.captureExecutionPaper(r.flashPrefix.Version, job.discovered.Add(250*time.Millisecond))
		result := r.executionPaper.evaluate(t.Context(), <-r.executionPaper.jobs)
		if result.Status != "censored" || result.Error != "later prefix crosses block or replaces original branch" {
			t.Fatal(result)
		}
	}
}

func TestPaperDelayShutdownPersistsWholeGroup(t *testing.T) {
	r, job := delayFixture()
	p := r.executionPaper
	p.done = make(chan struct{})
	p.policy.TimeoutMS = 100
	p.chain = domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	// Retain a valid original prefix for scheduling but make local route assembly
	// unsupported, so this test has no RPC dependencies.
	job.event.Job.View = nil
	path := filepath.Join(t.TempDir(), "paper.jsonl")
	file, err := openPaperJournal(path, p.chain, domain.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	p.cancel = cancel
	p.schedule(job)
	cancel()
	go p.run(ctx, file)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitPaperLines(raw)
	if len(lines) != 4 || p.busy.Load() || p.remaining.Load() != 0 {
		t.Fatalf("lost group: %s", raw)
	}
	for _, line := range lines[2:] {
		var result paperExecutionResult
		if err := json.Unmarshal(line, &result); err != nil || result.Status != "censored" {
			t.Fatal(result, err)
		}
	}
}

func TestPaperDelayPolicyBounds(t *testing.T) {
	for _, delays := range [][]int64{{0}, {-1}, {200, 200}, {600, 200}, {2001}, {1, 2, 3, 4, 5}} {
		if validatePaperDelays(delays) == nil {
			t.Fatal("accepted", delays)
		}
	}
	if err := validatePaperDelays([]int64{100, 200, 600, 2000}); err != nil {
		t.Fatal(err)
	}
}

func TestPaperDelayStopsGroupAfterUnverifiableBaseline(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paper.jsonl")
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	file, err := openPaperJournal(path, chain, domain.Hash{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	p := &executionPaper{chain: chain, policy: executionPaperPolicy{TimeoutMS: 100}, jobs: make(chan paperExecutionJob, 2), done: make(chan struct{}), cancel: cancel}
	p.remaining.Store(2)
	p.jobs <- paperExecutionJob{group: domain.Hash{1}, unavailable: "RPC unavailable"}
	p.jobs <- paperExecutionJob{group: domain.Hash{1}, delayMS: 200}
	go p.run(ctx, file)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitPaperLines(raw)
	var result paperExecutionResult
	if err := json.Unmarshal(lines[2], &result); err != nil || result.Status != "censored" || result.Error != "earlier scenario could not be verified; group stopped" {
		t.Fatal(result, err)
	}
}

func TestPaperDelayCapturesWaitingIntentWhileWorkerIsBusy(t *testing.T) {
	r, job := delayFixture()
	p := r.executionPaper
	r.reactive = &reactiveRuntime{}
	job.intent = &execution.ReactiveIntent{ID: domain.Hash{7}}
	r.reactive.offerVerification(&job)
	r.flashPrefix.Index++
	r.captureExecutionPaper(r.flashPrefix.Version, job.discovered.Add(250*time.Millisecond))
	if p.waiting == nil || len(p.waiting.captured) != 1 || len(p.jobs) != 0 {
		t.Fatal("later prefix of a waiting intent was not captured")
	}
	p.busy.Store(false) // The previous group finished.
	r.scheduleReactiveVerification()
	baseline, arrival := <-p.jobs, <-p.jobs
	if baseline.delayMS != 0 || baseline.intent == nil || arrival.delayMS != 200 || arrival.prefix == nil || arrival.group != baseline.group || p.pending == nil || p.waiting != nil {
		t.Fatalf("waiting group started incomplete: %+v %+v", baseline, arrival)
	}
	r.flashPrefix.Index++
	r.captureExecutionPaper(r.flashPrefix.Version, job.discovered.Add(650*time.Millisecond))
	if last := <-p.jobs; last.delayMS != 600 || last.prefix == nil || p.pending != nil {
		t.Fatal("running group lost its last scenario", last.unavailable)
	}
}
