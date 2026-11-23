package console

import (
	"encoding/json"
	"math/big"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

// The production case: a positive two-hop OP route whose research net is
// deeply negative because research charges other senders' observed costs.
func TestCandidatesShowSearchGrossAndKeepResearchAsLabeledEstimate(t *testing.T) {
	store := testStore(t)
	f := newReactiveFixture(t)
	f.send(f.won, f.intent(1), 40)
	importFixture(t, store, f)

	var output strings.Builder
	pools := []domain.PoolKey{{Address: domain.Address{0xaa}, Kind: domain.PoolKindV3}, {Address: domain.Address{0xbb}, Kind: domain.PoolKindVelodromeV2}}
	event := arb.SearchEvent{Job: arb.SearchJob{Version: domain.StateVersion{Source: domain.SourceFast, BlockNumber: 80}}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{
		{Cycle: graph.Cycle{Pools: pools, Start: f.asset, Len: 2}, Input: big.NewInt(1e18), Output: big.NewInt(1_006_200_000_000_000_000), Net: big.NewInt(6_200_000_000_000_000), GasCost: new(big.Int), NetAfterGas: big.NewInt(6_200_000_000_000_000)},
		{Cycle: graph.Cycle{Pools: pools, Start: domain.Address{0x51}, Len: 2}, Input: big.NewInt(10), Output: big.NewInt(13), Net: big.NewInt(3), GasCost: new(big.Int), NetAfterGas: big.NewInt(3)},
	}}}
	research := pipeline.ResearchEconomics{Basis: "observed_transaction_cost_scenario", Gross: big.NewInt(6_200_000_000_000_000), NetworkFee: big.NewInt(1_910_000_000_000_000_000), Net: big.NewInt(-1_903_800_000_000_000_000)}
	writer := pipeline.CandidateWriter{Economics: []pipeline.ResearchEconomics{research}, Encoder: json.NewEncoder(&output)}
	if err := writer.Consume(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "candidates.jsonl")
	appendFile(t, path, output.String())
	if err := store.trackFile(t.Context(), tailFile{Path: path, Instance: "one", RunID: "run-1", Kind: kindCandidate}); err != nil {
		t.Fatal(err)
	}
	if err := (&Tailer{store: store, instance: "one"}).Pass(t.Context(), "run-1"); err != nil {
		t.Fatal(err)
	}

	candidates, err := store.Candidates(t.Context(), "one", "fast", "gross", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates: %+v", candidates)
	}
	op := candidates[0]
	if op.Gross != "6200000000000000" || op.Asset != f.asset.Hex() || op.Hops != 2 || op.Block != 80 || op.Route[1].Kind != "Velodrome V2" {
		t.Fatalf("candidate: %+v", op)
	}
	// Valued at the fixture send's rate of two asset units per wei.
	if op.GrossNative == nil || *op.GrossNative != 3_100_000_000_000_000 {
		t.Fatalf("gross in wei: %v", op.GrossNative)
	}
	if op.Research == nil || op.Research.Net != "-1903800000000000000" || op.Research.Basis != "observed_transaction_cost_scenario" {
		t.Fatalf("research estimate: %+v", op.Research)
	}
	if other := candidates[1]; other.Research != nil || other.GrossNative != nil {
		t.Fatalf("candidate without research or rate: %+v", other)
	}

	h := newHarness(t, nil)
	h.signIn(t)
	if response := h.do(t, http.MethodGet, "/api/instances/one/candidates?sort=net", "", nil); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown sort: %d", response.StatusCode)
	}
	if response := h.do(t, http.MethodGet, "/api/instances/one/candidates?source=fast&sort=gross", "", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("candidates: %d", response.StatusCode)
	}
}
