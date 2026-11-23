package main

import (
	"bytes"
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
	"github.com/Rorical/Rosetta/internal/state"
)

func TestAutomaticAuditJoinsRealReceiptsAndPublicationDeadline(t *testing.T) {
	base := "../../testdata/golden/op-slipstream-cycle-158960654/"
	snapshot, err := os.ReadFile(base + "snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	store := state.New()
	if err = store.LoadSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base + "block.json")
	if err != nil {
		t.Fatal(err)
	}
	var block backtest.Record
	if err = json.Unmarshal(raw, &block); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(base + "expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Index uint64      `json:"transaction_index"`
		Cycle graph.Cycle `json:"cycle"`
	}
	if err = json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	// Attach a receipt-cost vector to the real swap fixture. The audit must
	// charge the whole transaction once, independently of its swap count.
	paidFee := big.NewInt(36217418558)
	block.Transactions[expected.Index].Fees = &domain.ReceiptFees{Family: domain.FamilyOPStack, Total: paidFee}
	dir := t.TempDir()
	research := filepath.Join(dir, "research.json")
	writeAuditJSON(t, research, map[string]any{"paper": map[string]any{"wrapped_native": expected.Cycle.Start.Hex(), "funding_basis": "own_capital_scenario"}})
	capture := filepath.Join(dir, "capture.jsonl")
	var buffer bytes.Buffer
	enc := json.NewEncoder(&buffer)
	if err = enc.Encode(backtest.Record{Type: "snapshot", ChainID: 10, Block: store.Block(), Snapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	if err = enc.Encode(block); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(capture, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	search := filepath.Join(dir, "search.json")
	writeAuditJSON(t, search, map[string]any{"k": 32, "max_depth": 6, "max_cycles": 10000, "funding": []any{map[string]any{"token": expected.Cycle.Start.Hex(), "min_input": "1", "seed_input": "26359137842449270", "max_input": "1000000000000000000"}}})
	version := domain.StateVersion{Source: domain.SourceFast, BlockNumber: block.Block.Number, Anchor: store.Block(), PrefixHash: domain.Hash{1}}
	first := pipeline.PrefixObservation{Version: version, FirstTxIndex: 0, TransactionCount: expected.Index, ReceivedAtNS: 100, AppliedAtNS: 150}
	for _, tx := range block.Transactions[:expected.Index] {
		first.Transactions = append(first.Transactions, tx.Hash)
	}
	last := pipeline.PrefixObservation{Version: version, FirstTxIndex: expected.Index, TransactionCount: 1, Transactions: []domain.Hash{block.Transactions[expected.Index].Hash}, ReceivedAtNS: 1000, AppliedAtNS: 1200}
	last.Version.PrefixHash = domain.Hash{2}
	for _, same := range []bool{false, true} {
		buffer.Reset()
		enc = json.NewEncoder(&buffer)
		frames := []pipeline.PrefixObservation{first, last}
		want := "observable"
		online := "publication_late"
		if same {
			merged := first
			merged.Transactions = append(append([]domain.Hash{}, first.Transactions...), last.Transactions...)
			merged.TransactionCount++
			frames = []pipeline.PrefixObservation{merged}
			want = "same_frame_no_window"
			online = "not_evaluated"
		}
		for _, frame := range frames {
			if err = enc.Encode(pipeline.ResearchEvent{Type: "prefix", ChainID: 10, Version: frame.Version, Details: frame}); err != nil {
				t.Fatal(err)
			}
		}
		paper := filepath.Join(dir, "paper.jsonl")
		if err = os.WriteFile(paper, buffer.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		buffer.Reset()
		if err = json.NewEncoder(&buffer).Encode(quoteEvidence{Opportunity: arb.Opportunity{Cycle: expected.Cycle, Net: big.NewInt(1)}, Timing: pipeline.QuoteTiming{BasisVersion: version, QuotedAtNS: 500, RecordedAtNS: 1100}}); err != nil {
			t.Fatal(err)
		}
		candidates := filepath.Join(dir, "candidates.jsonl")
		if err = os.WriteFile(candidates, buffer.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		execution := filepath.Join(dir, "execution.jsonl")
		buffer.Reset()
		execEncoder := json.NewEncoder(&buffer)
		if err = execEncoder.Encode(map[string]any{"type": "session", "schema": 1, "basis": "executor_simulation", "chain": domain.ChainIdentity{ChainID: 10}}); err != nil {
			t.Fatal(err)
		}
		if err = execEncoder.Encode(executionEvidence{Type: "execution", Schema: 1, Basis: "executor_simulation", Chain: domain.ChainIdentity{ChainID: 10}, ID: domain.Hash{9}, Version: version, Candidate: &arb.Opportunity{Cycle: expected.Cycle, Input: big.NewInt(20)}, Discovered: 500, Finished: 900, Status: "success", StateBasis: "discovery_snapshot", Economics: &pipeline.ResearchEconomics{Status: pipeline.NetEligible, Net: big.NewInt(10)}}); err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(execution, buffer.Bytes(), 0600); err != nil {
			t.Fatal(err)
		}
		buffer.Reset()
		if err = runCLI(t.Context(), []string{"-auto", "-capture", capture, "-search-config", search, "-research-config", research, "-paper", paper, "-candidates", candidates, "-execution-paper", execution}, &buffer); err != nil {
			t.Fatal(err)
		}
		matched := false
		for _, line := range bytes.Split(buffer.Bytes(), []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			var row struct {
				Execution  executionAuditResult `json:"execution_paper"`
				Type       string               `json:"type"`
				Cycle      backtest.ClosedCycle `json:"cycle"`
				Visibility backtest.Visibility  `json:"visibility"`
				Online     string               `json:"online_status"`
				PaidFee    *big.Int             `json:"paid_network_fee_in_asset"`
				PaidNet    *big.Int             `json:"swap_surplus_after_paid_network_fee"`
				FeeBasis   string               `json:"paid_fee_conversion_basis"`
			}
			if err = json.Unmarshal(line, &row); err != nil {
				t.Fatal(err)
			}
			if row.Type != "opportunity_audit" || row.Cycle.Transaction != block.Transactions[expected.Index].Hash {
				continue
			}
			matched = true
			if row.Visibility.Status != want || row.Online != online {
				t.Fatalf("same=%v visibility=%+v status=%s", same, row.Visibility, row.Online)
			}
			if !same && (row.Execution.Status != "sampled" || row.Execution.BaselinePositive != 1 || row.Execution.ReadyBeforeDeadline != 1) {
				t.Fatal("real receipt was not joined to execution paper", row.Execution)
			}
			if same && row.Execution.Status != "not_observable" {
				t.Fatal("same-frame execution counted as actionable", row.Execution)
			}
			wantNet := new(big.Int).Sub(row.Cycle.ActualGross, paidFee)
			if row.PaidFee == nil || row.PaidFee.Cmp(paidFee) != 0 || row.PaidNet == nil || row.PaidNet.Cmp(wantNet) != 0 || row.FeeBasis != "wrapped_native_1_to_1" {
				t.Fatalf("paid transaction cost was lost or charged repeatedly: %+v", row)
			}
		}
		if !matched {
			t.Fatal("real competitor not extracted")
		}
	}
}

func TestCanonicalFrameAllowsOlderVerifiedAnchor(t *testing.T) {
	origin := domain.BlockRef{Number: 10, Hash: domain.Hash{10}}
	parent := domain.BlockRef{Number: 11, Hash: domain.Hash{11}, ParentHash: origin.Hash}
	current := domain.BlockRef{Number: 12, Hash: domain.Hash{12}, ParentHash: parent.Hash}
	a := &automaticAudit{trace: traceData{frames: map[uint64][]backtest.PrefixEvidence{12: {{Version: domain.StateVersion{BlockNumber: 12, Anchor: origin}}}}}}
	a.rememberAnchor(origin)
	a.rememberAnchor(parent)
	if len(a.canonicalFrames(current)) != 1 {
		t.Fatal("log branch was required to wait for RPC parent")
	}
	wrong := current
	wrong.ParentHash = domain.Hash{99}
	if len(a.canonicalFrames(wrong)) != 0 {
		t.Fatal("unverified ancestry accepted")
	}
}

func TestRetainedChainExcludesOrphanedCompetitors(t *testing.T) {
	origin := domain.BlockRef{Number: 10, Hash: domain.Hash{10}}
	orphan := domain.BlockRef{Number: 11, Hash: domain.Hash{1}, ParentHash: origin.Hash}
	replacement := domain.BlockRef{Number: 11, Hash: domain.Hash{2}, ParentHash: origin.Hash}
	tip := domain.BlockRef{Number: 12, Hash: domain.Hash{12}, ParentHash: replacement.Hash}
	headers := map[domain.Hash]domain.BlockRef{origin.Hash: origin, orphan.Hash: orphan, replacement.Hash: replacement, tip.Hash: tip}
	selected := retainedChain(tip, headers)
	if selected[orphan.Hash] || len(selected) != 3 {
		t.Fatal("orphan entered denominator", selected)
	}
}

func TestDiagnosticRotatesIntoConfiguredFunding(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	keys := []domain.PoolKey{{Address: domain.Address{3}}, {Address: domain.Address{4}}}
	c := backtest.ClosedCycle{KnownRoute: backtest.KnownRoute{Cycle: graph.Cycle{Start: a, Pools: keys, Len: 2}, Input: big.NewInt(10), Tokens: []domain.Address{a, b}}, SwapInputs: []*big.Int{big.NewInt(10), big.NewInt(20)}}
	policy := pipeline.SearchPolicy{Funding: map[domain.Address]pipeline.SearchFunding{b: {Bounds: arb.InputBounds{Min: big.NewInt(1), Max: big.NewInt(100)}}}}
	got := diagnosticFunding(c, policy)
	if got.Cycle.Start != b || got.Cycle.Pools[0] != keys[1] || got.Input.Cmp(big.NewInt(20)) != 0 || c.Cycle.Pools[0] != keys[0] || c.Cycle.Start != a {
		t.Fatal("funding rotation", got)
	}
}

// A competitor first in the next block backruns the parent's last frame. The
// child's index-0 transaction arrives in the competitor's batch, so measuring
// only the child left a window of nanoseconds and a quote published at the
// parent's frame did not count. Measured from the parent's last frame, such
// "same-round" misses typically had a window of about 100 ms.
func TestAutomaticAuditCountsAParentBlockBackrun(t *testing.T) {
	base := "../../testdata/golden/op-slipstream-cycle-158960654/"
	snapshot, err := os.ReadFile(base + "snapshot.json")
	if err != nil {
		t.Fatal(err)
	}
	store := state.New()
	if err = store.LoadSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(base + "block.json")
	if err != nil {
		t.Fatal(err)
	}
	var parent backtest.Record
	if err = json.Unmarshal(raw, &parent); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(base + "expected.json")
	if err != nil {
		t.Fatal(err)
	}
	var expected struct {
		Index uint64      `json:"transaction_index"`
		Cycle graph.Cycle `json:"cycle"`
	}
	if err = json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}
	rival := parent.Transactions[expected.Index]
	rival.Index = 1
	parent.Transactions = parent.Transactions[:expected.Index]
	child := backtest.Record{Type: "block", ChainID: parent.ChainID, Block: domain.BlockRef{Number: parent.Block.Number + 1, Hash: domain.Hash{0xc1}, ParentHash: parent.Block.Hash, Timestamp: parent.Block.Timestamp + 1},
		Transactions: []ingest.Transaction{{Index: 0, Hash: domain.Hash{0xc0}, Successful: true}, rival}}
	dir := t.TempDir()
	research := filepath.Join(dir, "research.json")
	writeAuditJSON(t, research, map[string]any{"paper": map[string]any{"wrapped_native": expected.Cycle.Start.Hex(), "funding_basis": "own_capital_scenario"}})
	capture := filepath.Join(dir, "capture.jsonl")
	var buffer bytes.Buffer
	enc := json.NewEncoder(&buffer)
	for _, record := range []backtest.Record{{Type: "snapshot", ChainID: 10, Block: store.Block(), Snapshot: snapshot}, parent, child} {
		if err = enc.Encode(record); err != nil {
			t.Fatal(err)
		}
	}
	if err = os.WriteFile(capture, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	search := filepath.Join(dir, "search.json")
	writeAuditJSON(t, search, map[string]any{"k": 32, "max_depth": 6, "max_cycles": 10000, "funding": []any{map[string]any{"token": expected.Cycle.Start.Hex(), "min_input": "1", "seed_input": "26359137842449270", "max_input": "1000000000000000000"}}})
	trigger := domain.StateVersion{Source: domain.SourceFast, BlockNumber: parent.Block.Number, Anchor: store.Block(), PrefixHash: domain.Hash{1}}
	parentFrame := pipeline.PrefixObservation{Version: trigger, FirstTxIndex: 0, TransactionCount: expected.Index, ReceivedAtNS: 100, AppliedAtNS: 150}
	for _, tx := range parent.Transactions {
		parentFrame.Transactions = append(parentFrame.Transactions, tx.Hash)
	}
	next := domain.StateVersion{Source: domain.SourceFast, BlockNumber: child.Block.Number, Anchor: parent.Block, PrefixHash: domain.Hash{2}}
	internal := pipeline.PrefixObservation{Version: next, FirstTxIndex: 0, TransactionCount: 1, Transactions: []domain.Hash{{0xc0}}, ReceivedAtNS: 1000, AppliedAtNS: 1001}
	competitor := pipeline.PrefixObservation{Version: next, FirstTxIndex: 1, TransactionCount: 1, Transactions: []domain.Hash{rival.Hash}, ReceivedAtNS: 1002, AppliedAtNS: 1003}
	competitor.Version.PrefixHash = domain.Hash{3}
	buffer.Reset()
	enc = json.NewEncoder(&buffer)
	for _, frame := range []pipeline.PrefixObservation{parentFrame, internal, competitor} {
		if err = enc.Encode(pipeline.ResearchEvent{Type: "prefix", ChainID: 10, Version: frame.Version, Details: frame}); err != nil {
			t.Fatal(err)
		}
	}
	paper := filepath.Join(dir, "paper.jsonl")
	if err = os.WriteFile(paper, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	buffer.Reset()
	if err = json.NewEncoder(&buffer).Encode(quoteEvidence{Opportunity: arb.Opportunity{Cycle: expected.Cycle, Net: big.NewInt(1)}, Timing: pipeline.QuoteTiming{BasisVersion: trigger, QuotedAtNS: 400, RecordedAtNS: 500}}); err != nil {
		t.Fatal(err)
	}
	candidates := filepath.Join(dir, "candidates.jsonl")
	if err = os.WriteFile(candidates, buffer.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	buffer.Reset()
	if err = runCLI(t.Context(), []string{"-auto", "-capture", capture, "-search-config", search, "-research-config", research, "-paper", paper, "-candidates", candidates}, &buffer); err != nil {
		t.Fatal(err)
	}
	matched := false
	for _, line := range bytes.Split(buffer.Bytes(), []byte("\n")) {
		var row struct {
			Type       string               `json:"type"`
			Cycle      backtest.ClosedCycle `json:"cycle"`
			Visibility backtest.Visibility  `json:"visibility"`
			Online     string               `json:"online_status"`
			Reason     string               `json:"miss_reason"`
		}
		if len(line) == 0 || json.Unmarshal(line, &row) != nil || row.Type != "opportunity_audit" || row.Cycle.Transaction != rival.Hash {
			continue
		}
		matched = true
		if row.Visibility.Status != "observable" || !row.Visibility.Parent || row.Visibility.Window != 1002-150 || row.Visibility.Version != trigger || row.Online != "reported" {
			t.Fatalf("visibility=%+v online=%s reason=%s", row.Visibility, row.Online, row.Reason)
		}
	}
	if !matched {
		t.Fatal("competitor not extracted")
	}
}
