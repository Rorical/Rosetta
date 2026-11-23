package remote

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
	"math/big"
	"testing"
	"time"
)

func TestPaperRejectsKnownTaxBeforeSpendingSamplingSlot(t *testing.T) {
	store, pools := poolFixture(t)
	risks, err := tokenrisk.Open("", domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer risks.Close()
	if err = risks.Record(tokenrisk.Evidence{Token: pools[0].Token1, Requested: big.NewInt(1000), Received: big.NewInt(990), Block: domain.BlockRef{Hash: domain.Hash{1}}, Source: "executor_trace"}); err != nil {
		t.Fatal(err)
	}
	p := &executionPaper{risks: risks, jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{WrappedNative: pools[0].Token0.Hex(), IntervalMS: 1000}}
	o := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(1000), Net: big.NewInt(100)}
	r := &runtime{executionPaper: p, store: store}
	event := arb.SearchEvent{Stage: arb.QuoteOptimized, Job: arb.SearchJob{View: store.View()}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{o}}}
	if err = r.offerExecutionPaper(event); err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 0 || p.busy.Load() || !p.next.IsZero() {
		t.Fatal("taxed token consumed paper capacity")
	}
	summary := p.sampler.drain()
	if summary == nil || summary.Buckets[0].Reasons["token_transfer_shortfall"] != 1 {
		t.Fatal(summary)
	}
}

func TestCachedFeeRejectsThinProfitWithoutRPCAndExpires(t *testing.T) {
	store, pools := poolFixture(t)
	wrapped := pools[0].Token0
	now := time.Now()
	o := arb.Opportunity{Cycle: graph.Cycle{Start: wrapped, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(1000), Net: big.NewInt(100), Legs: []arb.Leg{{Pool: pools[0].Key, AmountOut: big.NewInt(2000)}, {Pool: pools[1].Key, AmountOut: big.NewInt(1100)}}}
	p := &executionPaper{policy: executionPaperPolicy{WrappedNative: wrapped.Hex(), GasLimit: 100000, TipWei: "0"}}
	cache := &paperAdmissionFee{extra: big.NewInt(100), maxBytes: 4096, expires: now.Add(time.Second), block: 10}
	p.admissionFee.Store(cache)
	r := &runtime{executionPaper: p, block: rpc.ExecutionBlock{BaseFee: big.NewInt(1)}}
	event := arb.SearchEvent{Job: arb.SearchJob{View: store.View(), Version: domain.StateVersion{BlockNumber: 10}}}
	choice := planning.FundingChoice{Asset: planning.AssetPolicy{MaxLoanFee: new(big.Int)}}
	if reason := r.cachedPaperFee(event, o, choice, now); reason != "network_fee_exceeds_gross" {
		t.Fatal(reason)
	}
	if reason := r.cachedPaperFee(event, o, choice, now.Add(2*time.Second)); reason != "" {
		t.Fatal("stale fee reused", reason)
	}
}
