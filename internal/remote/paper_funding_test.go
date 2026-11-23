package remote

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestPaperSelectionSkipsUnfundableLeaderWithoutConsumingSlot(t *testing.T) {
	for _, priority := range []bool{false, true} {
		store, pools := poolFixture(t)
		wrapped := pools[0].Token0
		p := &executionPaper{jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{WrappedNative: wrapped.Hex(), IntervalMS: 30000}}
		if priority {
			if err := p.priority.configure(&paperPriorityPolicy{"1", 2, 1000, 30000}); err != nil {
				t.Fatal(err)
			}
		}
		keys := []domain.PoolKey{pools[0].Key, pools[1].Key}
		unfundable := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token1, Pools: keys, Len: 2}, Input: big.NewInt(10000), Net: big.NewInt(1000)}
		fundable := unfundable
		fundable.Cycle.Start, fundable.Net = wrapped, big.NewInt(100)
		r := &runtime{executionPaper: p, store: store, block: rpc.ExecutionBlock{Ref: store.Block()}}
		event := arb.SearchEvent{Stage: arb.QuoteOptimized, Job: arb.SearchJob{View: store.View(), Version: domain.StateVersion{Source: domain.SourceStandard, Anchor: store.Block()}}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{unfundable, fundable}}}
		if err := r.offerExecutionPaper(event); err != nil {
			t.Fatal(err)
		}
		if len(p.jobs) != 1 {
			t.Fatal("fundable alternative was not admitted")
		}
		if job := <-p.jobs; job.opportunity.Cycle.Start != wrapped {
			t.Fatal("unfundable leader consumed the execution slot")
		}
		summary := p.sampler.drain()
		if summary == nil || summary.Buckets[0].Reasons["unsupported_funding"] != 1 {
			t.Fatal("funding exclusion was not recorded", summary)
		}
	}
}

func TestPaperSelectionRejectsLoanCostBeforeReservingBudget(t *testing.T) {
	store, pools := poolFixture(t)
	lender := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{90}}, Token0: pools[0].Token0, Token1: pools[0].Token1, Fee: 3000, TickSpacing: 60, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1000000)}
	if err := store.Upsert(lender); err != nil {
		t.Fatal(err)
	}
	quote := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token1, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(1000000), Net: big.NewInt(100)}
	wrapped := pools[0].Token0
	if _, err := paperFunding(t.Context(), quote, store.Pools(), wrapped); !errors.Is(err, planning.ErrFundingUnprofitable) {
		t.Fatal("known loan cost was not rejected", err)
	}
	p := &executionPaper{jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{IntervalMS: 30000, WrappedNative: wrapped.Hex()}}
	if err := p.priority.configure(&paperPriorityPolicy{"1", 2, 1000, 30000}); err != nil {
		t.Fatal(err)
	}
	r := &runtime{executionPaper: p, store: store}
	event := arb.SearchEvent{Stage: arb.QuoteOptimized, Job: arb.SearchJob{View: store.View()}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}}
	if err := r.offerExecutionPaper(event); err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 0 || p.busy.Load() || !p.next.IsZero() || len(p.priority.admitted) != 0 {
		t.Fatal("funding rejection consumed sampling capacity")
	}
	if summary := p.sampler.drain(); summary == nil || summary.Buckets[0].Reasons["funding_cost_exceeds_gross"] != 1 {
		t.Fatal("missing precise loan-cost rejection", summary)
	}
	// Callback funding does not pay an independent lender's flash fee.
	quote.Cycle.Pools[0] = lender.Key
	if _, err := paperFunding(t.Context(), quote, store.Pools(), wrapped); err != nil {
		t.Fatal("callback funding was rejected with independent lender", err)
	}
}
