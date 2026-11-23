package arb

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestParallelSizingPublishesNeighbourWhileEarlierPathIsBlocked(t *testing.T) {
	w, job, q, _ := incrementalFixture(t, false)
	w.cpus = newSearchCPUsOf(2)
	if err := w.Submit(job); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.entered)
	early := nextSearch(t, w)
	if !early.Partial || slices.Contains(early.Result.Opportunities[0].Cycle.Pools, q.block) {
		t.Fatal("independent path waited for blocked path")
	}
	next := job
	next.Dirty = []domain.PoolKey{q.block}
	next.Version.TxIndex++
	if err := w.Submit(next); err != nil {
		t.Fatal(err)
	}
	waitSignal(t, q.canceled)
	if !w.Rebase(&early) || !w.Accept(early) {
		t.Fatal("changed path invalidated independent result")
	}
	releaseSearch(early.Job)
}

func TestParallelRecordedSearchMatchesSerialQuotes(t *testing.T) {
	engine, view, dirty, opts := recordedSearchFixture(t)
	expected, err := engine.EvaluateRanked(t.Context(), view, dirty, opts)
	if err != nil || len(expected.Opportunities) == 0 {
		t.Fatal("recorded reference", err)
	}
	for _, workers := range []int{1, 2, 4, 8} {
		ctx, cancel := context.WithCancel(t.Context())
		w, err := NewWorker(ctx, engine, opts)
		if err != nil {
			t.Fatal(err)
		}
		w.cpus = newSearchCPUsOf(workers)
		job := SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}
		if err := w.Submit(job); err != nil {
			t.Fatal(err)
		}
		var actual []Opportunity
		for {
			event := nextSearch(t, w)
			if event.Err != nil || !w.Rebase(&event) || !w.Accept(event) {
				t.Fatal("parallel recorded search", event.Err)
			}
			actual = append(actual, event.Result.Opportunities...)
			releaseSearch(event.Job)
			if !event.Partial {
				break
			}
		}
		slices.SortFunc(actual, func(a, b Opportunity) int {
			if order := slices.Compare(a.Cycle.Start[:], b.Cycle.Start[:]); order != 0 {
				return order
			}
			return compareOpportunities(a, b)
		})
		if !reflect.DeepEqual(actual, expected.Opportunities) {
			t.Fatal("parallel sizing changed recorded quotes", workers)
		}
		cancel()
		<-w.Done()
	}
}

func TestSearchCPUBudgetCancelsWaitersWithoutLeakingCapacity(t *testing.T) {
	cpus := newSearchCPUsOf(2)
	held, err := cpus.acquire(t.Context(), 8)
	if err != nil || held.count != 2 {
		t.Fatal("budget was not bounded", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() { lease, err := cpus.acquire(ctx, 1); lease.release(); result <- err }()
	cancel()
	if err := <-result; err != context.Canceled {
		t.Fatal("blocked lane ignored cancellation", err)
	}
	held.release()
	held.release()
	reused, err := cpus.acquire(t.Context(), 2)
	if err != nil || reused.count != 2 {
		t.Fatal("cancellation leaked CPU capacity", err)
	}
	reused.release()
}

func TestParallelCacheHitSkipsTickSnapshotCopies(t *testing.T) {
	engine, view, selection := exactBenchmarkFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	w, err := NewWorker(ctx, engine, rankedOptions(selection.candidates[0].cycle.Start, 1_000_000))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cancel(); <-w.Done() })
	job := SearchJob{Incremental: true, View: view, Dirty: selection.Pools(), GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1}}
	consume := func() SearchResult {
		if err := w.Submit(job); err != nil {
			t.Fatal(err)
		}
		for {
			e := nextSearch(t, w)
			if e.Err != nil || !w.Rebase(&e) || !w.Accept(e) {
				t.Fatal("cached sizing", e.Err)
			}
			releaseSearch(e.Job)
			if !e.Partial {
				return e.Result
			}
		}
	}
	first := consume()
	reads := view.deepReads
	if first.Selected == 0 || reads != 2 {
		t.Fatal("tick fixture was not priced", reads)
	}
	job.Recheck = job.Dirty
	job.Dirty = nil
	job.Version.TxIndex++
	second := consume()
	if second.ExactReused != first.Selected || view.deepReads != reads {
		t.Fatal("cache hit copied ticks again", second.ExactReused, view.deepReads, reads)
	}
}
