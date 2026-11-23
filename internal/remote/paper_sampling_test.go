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

func TestPaperSamplerAlternatesProfitAndUnderrepresentedRoutes(t *testing.T) {
	wrapped := domain.Address{1}
	makeQuote := func(id byte, kind domain.PoolKind, hops int, profit int64) arb.Opportunity {
		keys := make([]domain.PoolKey, hops)
		for i := range keys {
			keys[i] = domain.PoolKey{Address: domain.Address{id, byte(i + 1)}, Kind: kind}
		}
		return arb.Opportunity{Cycle: graph.Cycle{Start: wrapped, Pools: keys, Len: hops}, Input: big.NewInt(10000), Net: big.NewInt(profit)}
	}
	quotes := []arb.Opportunity{makeQuote(1, domain.PoolKindV2, 2, 100), makeQuote(2, domain.PoolKindV3, 4, 20), makeQuote(3, domain.PoolKindWrap+1, 3, 10000)}
	event := arb.SearchEvent{Result: arb.SearchResult{Opportunities: quotes}}
	var sampler paperSampler
	for i, want := range []int{0, 1, 0, 1} {
		got, selection := sampler.chooseCandidate(event, nil, wrapped, nil)
		sampler.admit(selection, event.Result.Opportunities[got])
		if got != want || (i%2 == 0) != (selection.Mode == "native_gross_priority") {
			t.Fatalf("round %d got %d %+v", i, got, selection)
		}
	}
	sampler.skip(quotes[2], domain.StateVersion{BlockNumber: 12}, time.Unix(1, 0), "unsupported_executor_pool")
	summary := sampler.drain()
	if len(summary.Buckets) != 1 || summary.Buckets[0].Stratum.Supported || summary.Buckets[0].MaxGross.Int64() != 10000 {
		t.Fatal(summary)
	}
	if sampler.drain() != nil {
		t.Fatal("skip summary repeated")
	}
}
func TestPaperSamplerDoesNotCompareDifferentRawCurrencies(t *testing.T) {
	wrapped := domain.Address{1}
	keys := []domain.PoolKey{{Address: domain.Address{10}, Kind: domain.PoolKindV2}}
	known := arb.Opportunity{Cycle: graph.Cycle{Start: wrapped, Pools: keys}, Net: big.NewInt(10)}
	unknown := known
	unknown.Cycle.Start = domain.Address{2}
	unknown.Cycle.Pools = nil
	unknown.Net = new(big.Int).Lsh(big.NewInt(1), 200)
	var sampler paperSampler
	event := arb.SearchEvent{Result: arb.SearchResult{Opportunities: []arb.Opportunity{unknown, known}}}
	selected, first := sampler.chooseCandidate(event, nil, wrapped, nil)
	if selected != 1 {
		t.Fatal("raw units outranked known native value")
	}
	sampler.admit(first, event.Result.Opportunities[selected])
	selected, selection := sampler.chooseCandidate(event, nil, wrapped, nil)
	if selected != 0 || selection.NativeGross != nil {
		t.Fatal("unknown valuation lost diversity slot")
	}
}
func TestPaperSkipSummaryBoundAndOwnership(t *testing.T) {
	var sampler paperSampler
	for i := 0; i < 300; i++ {
		o := arb.Opportunity{Cycle: graph.Cycle{Start: domain.Address{byte(i), byte(i >> 8)}, Pools: []domain.PoolKey{{Kind: domain.PoolKindV2, Address: domain.Address{1}}}}, Net: big.NewInt(100)}
		sampler.skip(o, domain.StateVersion{}, time.Now(), "interval")
		o.Net.SetInt64(999)
		o.Cycle.Pools[0].Address = domain.Address{9}
	}
	summary := sampler.drain()
	if len(summary.Buckets) != paperStratumLimit || summary.Overflow != 44 {
		t.Fatal("unbounded summaries", len(summary.Buckets), summary.Overflow)
	}
	if summary.Buckets[0].MaxGross.Int64() != 100 || summary.Buckets[0].MaxCycle.Pools[0].Address != (domain.Address{1}) {
		t.Fatal("summary aliases input")
	}
}

func TestPaperSelectionWindowKeepsLargerLaterQuoteAndOriginalTime(t *testing.T) {
	store, pools := poolFixture(t)
	quote := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(100), Net: big.NewInt(5)}
	p := &executionPaper{jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{IntervalMS: 1000, SamplingWindowMS: 100, WrappedNative: pools[0].Token0.Hex()}}
	r := &runtime{executionPaper: p, store: store, block: rpc.ExecutionBlock{Ref: store.Block()}}
	event := arb.SearchEvent{Stage: arb.QuoteOptimized, Job: arb.SearchJob{Version: domain.StateVersion{Source: domain.SourceStandard, Anchor: store.Block()}, View: store.View()}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}}
	if err := r.offerExecutionPaper(event); err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 0 || p.window == nil || !p.busy.Load() {
		t.Fatal("first partial quote executed before selection window")
	}
	first := p.window.job.discovered
	event.Result.Opportunities[0].Net = big.NewInt(50)
	if err := r.offerExecutionPaper(event); err != nil {
		t.Fatal(err)
	}
	selectedAt := p.window.job.discovered
	if selectedAt.Before(first) || p.window.job.opportunity.Net.Int64() != 50 {
		t.Fatal("later larger quote ignored")
	}
	p.flushSelection(p.window.until, true)
	job := <-p.jobs
	if job.opportunity.Net.Int64() != 50 || job.discovered != selectedAt || job.selection.WindowWaitNS <= 0 || p.sampler.rounds != 1 {
		t.Fatal("window changed discovery identity or counted intermediate selections")
	}
	summary := p.sampler.drain()
	if len(summary.Buckets) != 1 || summary.Buckets[0].Reasons["selection_window_replaced"] != 1 {
		t.Fatal(summary)
	}
}

func BenchmarkPaperSamplingSkipSummary(b *testing.B) {
	var sampler paperSampler
	o := arb.Opportunity{Cycle: graph.Cycle{Start: domain.Address{1}, Pools: []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{10}}, {Kind: domain.PoolKindV2, Address: domain.Address{11}}}}, Net: big.NewInt(100)}
	now := time.Now()
	b.ReportAllocs()
	for range b.N {
		sampler.skip(o, domain.StateVersion{}, now, "interval")
	}
}

func TestPaperSkipMaximumRetainsItsOwnReasonAmountAndVersion(t *testing.T) {
	var sampler paperSampler
	quote := arb.Opportunity{Cycle: graph.Cycle{Start: domain.Address{1}, Pools: []domain.PoolKey{{Kind: domain.PoolKindV2, Address: domain.Address{2}}}}, Input: big.NewInt(10), Net: big.NewInt(50)}
	first := domain.StateVersion{BlockNumber: 10}
	sampler.skip(quote, first, time.Unix(10, 0), "interval")
	quote.Input.SetInt64(999)
	quote.Net = big.NewInt(40)
	sampler.skip(quote, domain.StateVersion{BlockNumber: 11}, time.Unix(11, 0), "worker_busy")
	b := sampler.drain().Buckets[0]
	if b.MaxReason != "interval" || b.MaxInput.Int64() != 10 || b.Version != first || b.AtNS != time.Unix(10, 0).UnixNano() || b.Reasons["worker_busy"] != 1 {
		t.Fatal("maximum inherited an unrelated skip reason or amount", b)
	}
}
