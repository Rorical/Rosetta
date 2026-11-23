package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestPaperHighGrossBypassesIntervalWithoutDelayingRegularSample(t *testing.T) {
	store, pools := poolFixture(t)
	p := &executionPaper{jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{IntervalMS: 30000, SamplingWindowMS: 100, WrappedNative: pools[0].Token0.Hex(), Priority: &paperPriorityPolicy{MinNativeGrossWei: "100", MaxPerMinute: 2, MinIntervalMS: 1000, RouteCooldownMS: 30000}}}
	if err := p.priority.configure(p.policy.Priority); err != nil {
		t.Fatal(err)
	}
	r := &runtime{executionPaper: p, store: store, block: rpc.ExecutionBlock{Ref: store.Block()}}
	quote := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(1000), Net: big.NewInt(101)}
	event := arb.SearchEvent{Stage: arb.QuoteInitial, Job: arb.SearchJob{Version: domain.StateVersion{Source: domain.SourceStandard, Anchor: store.Block()}, View: store.View()}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}}
	p.next = time.Now().Add(30 * time.Second)
	next := p.next
	if err := r.offerExecutionPaper(event); err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 1 || p.window != nil {
		t.Fatal("high-gross initial quote blocked by interval or selection wait")
	}
	job := <-p.jobs
	if job.selection.Mode != "high_gross_priority" || !p.next.Equal(next) {
		t.Fatal("priority sample changed regular sampling budget")
	}
	p.busy.Store(false)
	event.Stage = arb.QuoteOptimized
	if err := r.offerExecutionPaper(event); err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 0 {
		t.Fatal("same route consumed another priority slot")
	}
	summary := p.sampler.drain()
	if summary == nil || summary.Buckets[0].Reasons["priority_route_cooldown"] != 1 {
		t.Fatal("missing exact suppression reason", summary)
	}
}

func TestPaperPriorityRollingBudgetAndRouteCooldown(t *testing.T) {
	var gate paperPriorityGate
	if err := gate.configure(&paperPriorityPolicy{MinNativeGrossWei: "100", MaxPerMinute: 2, MinIntervalMS: 1000, RouteCooldownMS: 30000}); err != nil {
		t.Fatal(err)
	}
	start := time.Unix(100, 0)
	a, b, c := domain.Hash{1}, domain.Hash{2}, domain.Hash{3}
	if reason := gate.reason(start, a); reason != "" {
		t.Fatal(reason)
	}
	gate.admit(start, a)
	if gate.reason(start.Add(time.Second), a) != "priority_route_cooldown" {
		t.Fatal("route cooldown not enforced")
	}
	if gate.reason(start.Add(time.Millisecond), b) != "priority_interval" {
		t.Fatal("burst not bounded")
	}
	gate.admit(start.Add(time.Second), b)
	if gate.reason(start.Add(59*time.Second), c) != "priority_budget" {
		t.Fatal("rolling budget not enforced")
	}
	if gate.reason(start.Add(time.Minute), c) != "" {
		t.Fatal("budget did not expire")
	}
}

func TestPaperPriorityPreservesBackoffAndRunningGroup(t *testing.T) {
	for _, reason := range []string{"backoff", "worker_busy"} {
		t.Run(reason, func(t *testing.T) {
			store, pools := poolFixture(t)
			p := &executionPaper{jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{WrappedNative: pools[0].Token0.Hex()}}
			if err := p.priority.configure(&paperPriorityPolicy{MinNativeGrossWei: "1", MaxPerMinute: 2, MinIntervalMS: 1000, RouteCooldownMS: 30000}); err != nil {
				t.Fatal(err)
			}
			if reason == "backoff" {
				p.backoff.Store(time.Now().Add(time.Minute).UnixMilli())
			} else {
				p.busy.Store(true)
			}
			r := &runtime{executionPaper: p, store: store, block: rpc.ExecutionBlock{Ref: store.Block()}}
			o := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(100), Net: big.NewInt(10000)}
			err := r.offerExecutionPaper(arb.SearchEvent{Stage: arb.QuoteInitial, Job: arb.SearchJob{View: store.View()}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{o}}})
			if err != nil || len(p.jobs) != 0 || len(p.priority.admitted) != 0 {
				t.Fatal("priority bypassed operational limit", err)
			}
			if p.sampler.drain().Buckets[0].Reasons[reason] != 1 {
				t.Fatal("missing skip reason")
			}
		})
	}
}

func TestPaperPriorityPolicyValidation(t *testing.T) {
	for _, policy := range []paperPriorityPolicy{
		{"0", 2, 1000, 30000}, {"100", 0, 1000, 30000}, {"100", 61, 1000, 30000}, {"100", 2, 0, 30000}, {"100", 2, 5000, 1000},
	} {
		var gate paperPriorityGate
		if gate.configure(&policy) == nil {
			t.Fatalf("accepted unbounded priority policy: %+v", policy)
		}
	}
}
