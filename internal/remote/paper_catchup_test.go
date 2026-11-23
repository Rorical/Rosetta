package remote

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestPaperEvidenceTracksCanonicalCatchup(t *testing.T) {
	store, pools := poolFixture(t)
	dir := t.TempDir()
	policy := filepath.Join(dir, "paper.json")
	data, _ := json.Marshal(map[string]any{"paper": map[string]any{"wrapped_native": pools[0].Token0.Hex(), "funding_basis": "own_capital_scenario"}, "swap_audit": map[string]any{"max_swaps": 0}})
	if err := os.WriteFile(policy, data, 0600); err != nil {
		t.Fatal(err)
	}
	research, err := pipeline.NewResearch(t.Context(), policy, filepath.Join(dir, "observations.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer research.Close()
	r := &runtime{store: store, research: research}
	for index := range 3 {
		next := r.store.Block()
		next.Number++
		next.ParentHash, next.Hash = next.Hash, domain.Hash{byte(41 + index)}
		if err := r.acceptCanonical(t.Context(), canonicalUpdate{block: rpc.ExecutionBlock{Ref: next}, catchingUp: true, nearTip: index == 0}); err != nil {
			t.Fatal(err)
		}
		version := domain.StateVersion{Source: domain.SourceFast, Anchor: next, BlockNumber: next.Number + 1}
		quote := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(100), Net: big.NewInt(1)}
		got := research.Observe(arb.SearchEvent{Job: arb.SearchJob{View: r.store.View(), Version: version}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}})
		if len(got) != 1 || got[0].Calibration.EvidenceHead != next || got[0].Calibration.Reason != "insufficient_protocol_cost_samples" {
			t.Fatalf("catchup lost verified fee anchor: %+v", got)
		}
	}
}
