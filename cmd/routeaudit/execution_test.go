package main

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

func TestExecutionAuditMatchesCanonicalPrefixAndKeepsDelayedScenariosSeparate(t *testing.T) {
	token, other := domain.Address{1}, domain.Address{2}
	keys := []domain.PoolKey{{Address: domain.Address{3}}, {Address: domain.Address{4}}}
	cycle := graph.Cycle{Start: token, Pools: keys, Len: 2}
	version := domain.StateVersion{Source: domain.SourceFast, BlockNumber: 11, Anchor: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}, PrefixHash: domain.Hash{1}}
	frame := backtest.PrefixEvidence{Version: version, First: 0, Count: 1, Hashes: []domain.Hash{{1}}, Received: 100, Applied: 150}
	a := automaticAudit{block: backtest.Record{Block: domain.BlockRef{Number: 11, ParentHash: version.Anchor.Hash}, Transactions: []ingest.Transaction{{Index: 0, Hash: domain.Hash{1}}, {Index: 1, Hash: domain.Hash{2}}}}, trace: traceData{frames: map[uint64][]backtest.PrefixEvidence{11: {frame}}}}
	c := cycleAudit{cycle: backtest.ClosedCycle{KnownRoute: backtest.KnownRoute{Cycle: cycle, BeforeTx: 1}}, positive: map[uint64]bool{1: true}}
	v := backtest.Visibility{Status: "observable", Available: 150, Deadline: 1000}
	path := filepath.Join(t.TempDir(), "execution.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	write := func(row any) {
		t.Helper()
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	write(map[string]any{"type": "session", "schema": 1, "basis": "executor_simulation", "chain": domain.ChainIdentity{ChainID: 10}})
	sample := executionEvidence{Type: "execution", Schema: 1, Basis: "executor_simulation", Chain: domain.ChainIdentity{ChainID: 10}, ID: domain.Hash{1}, Group: domain.Hash{9}, Version: version, Candidate: &arb.Opportunity{Cycle: cycle, Input: big.NewInt(20)}, Status: "success", StateBasis: "discovery_snapshot", Discovered: 500, Finished: 2000, Economics: &pipeline.ResearchEconomics{Net: big.NewInt(-5)}}
	write(sample)
	write(sample) // Re-importing a sample must not create another fill.
	sample.ID = domain.Hash{2}
	sample.DelayMS = 200
	sample.StateBasis = "later_observed_prefix"
	sample.Economics.Net = big.NewInt(99)
	write(sample)
	sample.ID = domain.Hash{4}
	sample.DelayMS = 0
	sample.Status = "reverted"
	sample.StateBasis = "unverified"
	write(sample)
	sample.Status = "success"
	sample.StateBasis = "discovery_snapshot"
	sample.ID = domain.Hash{3}
	sample.DelayMS = 0
	sample.Version.PrefixHash = domain.Hash{99}
	sample.Economics.Net = big.NewInt(1000)
	write(sample)
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	trace, err := loadExecutionTrace(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	tokens := func(key domain.PoolKey) (domain.Address, domain.Address, bool) { return token, other, true }
	got := a.executionAudit(&c, v, trace, tokens)
	if got.Status != "sampled" || len(got.Samples) != 3 || got.BaselinePositive != 0 || got.BaselineNonPositive != 1 || got.BaselineUnknown != 1 || got.ReadyBeforeDeadline != 0 {
		t.Fatalf("wrong execution attribution: %+v", got)
	}
	// Same block and route on an orphan prefix cannot prove execution coverage.
	a.trace.frames[11][0].Hashes[0] = domain.Hash{99}
	got = a.executionAudit(&c, v, trace, tokens)
	if got.Status != "not_observed" {
		t.Fatal(got)
	}
}

func TestExecutionSkipNeedsExactMaximumWitness(t *testing.T) {
	token := domain.Address{1}
	cycle := graph.Cycle{Start: token, Pools: []domain.PoolKey{{Address: domain.Address{3}}, {Address: domain.Address{4}}}, Len: 2}
	version := domain.StateVersion{Source: domain.SourceFast, BlockNumber: 11, Anchor: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}}
	a := automaticAudit{block: backtest.Record{Block: domain.BlockRef{Number: 11, ParentHash: version.Anchor.Hash}, Transactions: []ingest.Transaction{{Index: 0, Hash: domain.Hash{1}}, {Index: 1, Hash: domain.Hash{2}}}}, trace: traceData{frames: map[uint64][]backtest.PrefixEvidence{11: {{Version: version, First: 0, Count: 1, Hashes: []domain.Hash{{1}}, Received: 100, Applied: 150}}}}}
	c := cycleAudit{cycle: backtest.ClosedCycle{KnownRoute: backtest.KnownRoute{Cycle: cycle, BeforeTx: 1}}, positive: map[uint64]bool{1: true}}
	v := backtest.Visibility{Status: "observable", Available: 150, Deadline: 1000}
	tokens := func(domain.PoolKey) (domain.Address, domain.Address, bool) { return token, domain.Address{2}, true }
	trace := executionTrace{Provided: true, Skips: []skipEvidence{{Cycle: cycle, Version: version, AtNS: 500, Reason: "interval", Input: big.NewInt(10), Gross: big.NewInt(5)}}}
	got := a.executionAudit(&c, v, trace, tokens)
	if got.Status != "sampling_skipped" || len(got.Skips) != 1 || got.Skips[0].Reason != "interval" {
		t.Fatal(got)
	}
	trace.Skips[0].Reason = "" // Old aggregate reasons do not identify why this particular quote was skipped.
	got = a.executionAudit(&c, v, trace, tokens)
	if got.Status != "not_observed" {
		t.Fatal(got)
	}
}

func TestRecallFunnelExcludesUnknownCostsAndUnobservableWindows(t *testing.T) {
	a := automaticAudit{}
	c := cycleAudit{cycle: backtest.ClosedCycle{KnownRoute: backtest.KnownRoute{Cycle: graph.Cycle{Start: domain.Address{1}}}, Transaction: domain.Hash{1}, ActualGross: big.NewInt(100)}}
	v := backtest.Visibility{Status: "observable"}
	skipped := executionAuditResult{Status: "sampling_skipped"}
	a.recordFunnel(&c, v, "reported", "reported", auditAttribution{Reason: "reported"}, nil, skipped)
	a.recordFunnel(&c, backtest.Visibility{Status: "same_frame_no_window"}, "not_evaluated", "window_unknown", auditAttribution{}, big.NewInt(99), skipped)
	a.recordFunnel(&c, v, "reported", "reported", auditAttribution{Reason: "reported"}, big.NewInt(-1), skipped)
	a.recordFunnel(&c, v, "reported", "reported", auditAttribution{Reason: "reported"}, big.NewInt(50), skipped)
	got := a.funnel[domain.Address{1}.Hex()]
	if got.ObservablePositiveAfterPaidFee != 1 || got.UnknownPaidFee != 1 || got.NotObservable != 1 || got.NonPositiveAfterPaidFee != 1 || got.PaperSkipped != 1 || len(got.Largest) != 1 || got.Largest[0].Reason != "paper_sampling_skipped" || got.Largest[0].PaidNet.Int64() != 50 {
		t.Fatalf("biased recall denominator: %+v", got)
	}
}

func TestExecutionJournalRejectsCrossChainAndConflictingSamples(t *testing.T) {
	for _, conflict := range []bool{false, true} {
		t.Run(map[bool]string{false: "chain", true: "sample"}[conflict], func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "execution.jsonl")
			f, err := os.Create(path)
			if err != nil {
				t.Fatal(err)
			}
			enc := json.NewEncoder(f)
			enc.Encode(map[string]any{"type": "session", "schema": 1, "basis": "executor_simulation", "chain": domain.ChainIdentity{ChainID: 10}})
			row := executionEvidence{Type: "execution", Schema: 1, Basis: "executor_simulation", Chain: domain.ChainIdentity{ChainID: 10}, ID: domain.Hash{1}, Status: "success"}
			enc.Encode(row)
			if conflict {
				row.Status = "reverted"
			} else {
				row.Chain.ChainID = 42161
			}
			enc.Encode(row)
			f.Close()
			if _, err := loadExecutionTrace(path, 10); err == nil {
				t.Fatal("ambiguous execution evidence accepted")
			}
		})
	}
}

func TestLegacySkipSummaryOnlyAttributesSingleReasonBuckets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "legacy.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	enc := json.NewEncoder(f)
	for _, row := range []any{
		map[string]any{"type": "session", "schema": 1, "basis": "executor_simulation", "chain": domain.ChainIdentity{ChainID: 10}},
		map[string]any{"type": "sampling_summary", "basis": "executor_simulation", "buckets": []any{map[string]any{"reasons": map[string]int{"interval": 4}}, map[string]any{"reasons": map[string]int{"interval": 3, "worker_busy": 1}}}},
	} {
		if err := enc.Encode(row); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	trace, err := loadExecutionTrace(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(trace.Skips) != 2 || trace.Skips[0].Reason != "interval" || trace.Skips[1].Reason != "" {
		t.Fatal("legacy aggregate invented a per-route skip reason", trace)
	}
}

func TestPositiveHistoricalSimulationDoesNotHideLateConfirmation(t *testing.T) {
	a := automaticAudit{}
	c := cycleAudit{cycle: backtest.ClosedCycle{KnownRoute: backtest.KnownRoute{Cycle: graph.Cycle{Start: domain.Address{1}}}, ActualGross: big.NewInt(100)}}
	a.recordFunnel(&c, backtest.Visibility{Status: "observable"}, "reported", "reported", auditAttribution{Reason: "reported"}, big.NewInt(50), executionAuditResult{Status: "sampled", BaselinePositive: 1})
	got := a.funnel[domain.Address{1}.Hex()]
	if got.PaperPositive != 1 || got.PaperReady != 0 || len(got.Largest) != 1 || got.Largest[0].Reason != "paper_confirmation_after_window" {
		t.Fatal("late historical success was counted as timely capture", got)
	}
}
