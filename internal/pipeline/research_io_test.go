package pipeline

import (
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestResearchCloseDrainsCandidatesAndRecordsCensoredGaps(t *testing.T) {
	store, topology, cycle, version := researchFixture(t)
	quote, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "observations.jsonl")
	r, err := NewResearch(context.Background(), "", path, domain.ChainIdentity{ChainID: 10})
	if err != nil {
		t.Fatal(err)
	}
	r.Observe(arb.SearchEvent{Job: arb.SearchJob{Version: version, GraphVersion: 1}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}})
	r.skipped.Add(3)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	kinds := map[string]int{}
	for decoder.More() {
		var event ResearchEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.ChainID != 10 {
			t.Fatal("cross-chain observation")
		}
		kinds[event.Type]++
		if event.Type == "ended" && (!event.Censored || event.Continuous) {
			t.Fatal("gap was recorded as continuous", event)
		}
	}
	if kinds["session"] != 1 || kinds["candidate"] != 1 || kinds["gap"] != 1 || kinds["ended"] != 1 {
		t.Fatal(kinds)
	}
}
func TestResearchIgnoresFramesQueuedBeforeCandidateObservation(t *testing.T) {
	store, topology, cycle, version := researchFixture(t)
	quote, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Unix(100, 0)
	var events []ResearchEvent
	s := researchState{policy: ResearchPolicy{MaxTracked: 1, MaxAgeMS: 1000}, emit: func(event ResearchEvent) error { events = append(events, event); return nil }}
	if err := s.discover(arb.SearchEvent{Job: arb.SearchJob{Version: version}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{quote}}}, at); err != nil {
		t.Fatal(err)
	}
	version.TxIndex++
	if err := s.advance(t.Context(), ResearchFrame{View: store.View(), Version: version, At: at.Add(-time.Millisecond), Quote: topology.Engine.QuoteCycle}); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatal("older frame changed candidate lifetime", events)
	}
}

func TestSwapAuditDropsDoNotCensorCandidateLifetime(t *testing.T) {
	path := filepath.Join(t.TempDir(), "observations.jsonl")
	r, err := NewResearch(context.Background(), "", path)
	if err != nil {
		t.Fatal(err)
	}
	r.auditSkipped.Add(3)
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	found := false
	for decoder.More() {
		var event ResearchEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatal(err)
		}
		if event.Type == "gap" {
			t.Fatal("audit loss became a candidate observation gap")
		}
		if event.Type == "swap_quote_audit_gap" {
			found = event.Censored && event.Skipped == 3
		}
	}
	if !found {
		t.Fatal("missing bounded audit gap")
	}
}
