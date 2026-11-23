package pipeline

import (
	"bytes"
	"encoding/json"
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
)

func TestInitialAndOptimizedIdenticalQuotesBothPublishWithComputeTiming(t *testing.T) {
	_, _, cycle, version := researchFixture(t)
	trigger := time.Unix(100, 0)
	event := arb.SearchEvent{Partial: true, Stage: arb.QuoteInitial, QuotedAt: trigger.Add(2 * time.Millisecond), Job: arb.SearchJob{Version: version, ReceivedAt: trigger}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{{Cycle: cycle, Input: big.NewInt(10), Output: big.NewInt(11), Net: big.NewInt(1)}}}}
	scheduler := SearchScheduler{}
	if !scheduler.freshCandidate(event) || scheduler.freshCandidate(event) {
		t.Fatal("initial dedup")
	}
	event.Stage = arb.QuoteOptimized
	if !scheduler.freshCandidate(event) {
		t.Fatal("identical optimized quote lost confirmation admission")
	}
	timing := quoteTiming(event, trigger.Add(20*time.Millisecond))
	if timing.ReceiveToQuoteUS != 2000 || timing.RecordedAtNS-timing.QuotedAtNS != int64(18*time.Millisecond) {
		t.Fatal("consumer delay counted as computation", timing)
	}
	var output bytes.Buffer
	if err := (CandidateWriter{Encoder: json.NewEncoder(&output)}).Consume(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	var row struct {
		Timing QuoteTiming `json:"timing"`
	}
	if err := json.Unmarshal(output.Bytes(), &row); err != nil || row.Timing.Stage != arb.QuoteOptimized {
		t.Fatal("paper output omitted stage", err)
	}
	var events []ResearchEvent
	state := researchState{policy: ResearchPolicy{MaxTracked: 2}, emit: func(e ResearchEvent) error { events = append(events, e); return nil }}
	event.Stage = arb.QuoteInitial
	if err := state.discover(event, trigger.Add(20*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	event.Stage = arb.QuoteOptimized
	if err := state.discover(event, trigger.Add(30*time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Type != "candidate" || events[1].Type != "quote_update" || events[1].Timing.Stage != arb.QuoteOptimized {
		t.Fatal("optimization was lost or counted as a new opportunity", events)
	}
}
