package pipeline

import (
	"context"
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func researchFixture(t *testing.T) (*state.Store, *Topology, graph.Cycle, domain.StateVersion) {
	t.Helper()
	store := state.New()
	a, b := domain.Address{1}, domain.Address{2}
	for i, reserve := range []int64{2000000, 1000000} {
		p := pool.State{Key: domain.PoolKey{Address: domain.Address{byte(10 + i)}, Kind: domain.PoolKindV2}, Token0: a, Token1: b, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(reserve), Fee: 3000}
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	topology, err := BuildTopology(t.Context(), store.Pools(), TopologyOptions{Depth: 2, K: 8, MaxCycles: 100, Notional: func(domain.Address) *big.Int { return big.NewInt(100) }})
	if err != nil {
		t.Fatal(err)
	}
	cycle := graph.Cycle{Start: a, Len: 2, Pools: []domain.PoolKey{store.Pools()[0].Key, store.Pools()[1].Key}}
	version := domain.StateVersion{Source: domain.SourceFast, Epoch: 1, Anchor: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, BlockNumber: 11, TxIndex: 1, PrefixHash: domain.Hash{1}}
	return store, topology, cycle, version
}
func TestResearchTracksFixedInputAndCensorsUnknownQuotes(t *testing.T) {
	store, topology, cycle, version := researchFixture(t)
	at := time.Unix(1000, 0)
	policy := ResearchPolicy{MaxTracked: 8, MaxAgeMS: 1000, EvaluationMS: 20}
	var output []ResearchEvent
	s := researchState{policy: policy, emit: func(event ResearchEvent) error { output = append(output, event); return nil }}
	quote, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100))
	if err != nil || quote.Net.Sign() <= 0 {
		t.Fatal(quote, err)
	}
	event := arb.SearchEvent{Job: arb.SearchJob{Version: version, GraphVersion: 1, ReceivedAt: at.Add(-5 * time.Millisecond)}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}}
	if err := s.discover(event, at); err != nil {
		t.Fatal(err)
	}
	if output[0].Economics.Net != nil || output[0].Economics.Basis != "gross_only" || output[0].SearchAgeMS != 5 {
		t.Fatal("unknown gas became net profit", output[0])
	}
	next := version
	next.TxIndex++
	next.PrefixHash = domain.Hash{2}
	quoteAt := func(ctx context.Context, view arb.View, c graph.Cycle, input *big.Int) (arb.Opportunity, error) {
		if input.Cmp(big.NewInt(100)) != 0 {
			t.Fatal("monitor reoptimized input")
		}
		return topology.Engine.QuoteCycle(ctx, view, c, input)
	}
	if err := s.advance(t.Context(), ResearchFrame{View: store.View(), Version: next, Graph: 1, At: at.Add(200 * time.Millisecond), Quote: quoteAt}); err != nil {
		t.Fatal(err)
	}
	key := store.Pools()[1].Key
	p, _ := store.Pool(key)
	p.Reserve0 = big.NewInt(1)
	p.Reserve1 = big.NewInt(1000000)
	if err := store.Upsert(p); err != nil {
		t.Fatal(err)
	}
	next.TxIndex++
	next.PrefixHash = domain.Hash{3}
	if err := s.advance(t.Context(), ResearchFrame{View: store.View(), Version: next, Graph: 1, At: at.Add(400 * time.Millisecond), Quote: quoteAt}); err != nil {
		t.Fatal(err)
	}
	last := output[len(output)-1]
	if last.Type != "ended" || last.Censored || last.PositiveSampleSpanMS != 200 || last.Reason != "profit_disappeared" {
		t.Fatal(last)
	}
}
func TestResearchCostsAreCeiledAndChargedOnce(t *testing.T) {
	token := domain.Address{1}
	policy := ResearchPolicy{Costs: []ResearchCost{{Token: token.Hex(), NetworkFee: "3", LoanFixed: "2", LoanFeePips: 100, SafetyBPS: 1, MinNetProfit: "1"}}}
	cost := policy.Economics(token, big.NewInt(101), big.NewInt(10))
	if cost.LoanFee.String() != "3" || cost.Safety.String() != "1" || cost.Net.String() != "3" || !cost.Eligible {
		t.Fatal(cost)
	}
}
func TestResearchPrefixResetIsCensoredRatherThanAProfitFailure(t *testing.T) {
	store, topology, cycle, version := researchFixture(t)
	at := time.Unix(1000, 0)
	quote, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	var output []ResearchEvent
	s := researchState{policy: ResearchPolicy{MaxTracked: 8, MaxAgeMS: 1000}, emit: func(e ResearchEvent) error { output = append(output, e); return nil }}
	if err := s.discover(arb.SearchEvent{Job: arb.SearchJob{Version: version, GraphVersion: 1}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}}, at); err != nil {
		t.Fatal(err)
	}
	version.Epoch++
	if err := s.advance(t.Context(), ResearchFrame{View: store.View(), Version: version, Graph: 1, At: at.Add(time.Millisecond), Quote: topology.Engine.QuoteCycle}); err != nil {
		t.Fatal(err)
	}
	if last := output[len(output)-1]; !last.Censored || last.Reason != "prefix_reset" {
		t.Fatal(last)
	}
}
