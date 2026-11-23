package remote

import (
	"context"
	"encoding/json"
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rpc"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPaperJournalRestartIdentityAndTornTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "samples.jsonl")
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	artifact := domain.Hash{1}
	f, err := openPaperJournal(path, chain, artifact)
	if err != nil {
		t.Fatal(err)
	}
	sample := paperExecutionResult{Type: "execution", Status: "reverted", GasUsed: 123, ID: domain.Hash{2}}
	if err := json.NewEncoder(f).Encode(sample); err != nil {
		t.Fatal(err)
	}
	if _, err := openPaperJournal(path, chain, artifact); err == nil {
		t.Fatal("concurrent journal owner allowed")
	}
	if _, err := f.WriteString(`{"torn":`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	f, err = openPaperJournal(path, chain, artifact)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var lines []json.RawMessage
	for _, line := range splitPaperLines(raw) {
		lines = append(lines, line)
	}
	if len(lines) != 2 {
		t.Fatalf("completed sample lost or torn sample retained: %s", raw)
	}
	var restored paperExecutionResult
	if err := json.Unmarshal(lines[1], &restored); err != nil || restored.GasUsed != 123 || restored.Status != "reverted" {
		t.Fatal(restored, err)
	}
	chain.ChainID = 8453
	if _, err := openPaperJournal(path, chain, artifact); err == nil {
		t.Fatal("cross-chain samples accepted")
	}
}
func splitPaperLines(raw []byte) [][]byte {
	var result [][]byte
	start := 0
	for i, b := range raw {
		if b == '\n' {
			result = append(result, raw[start:i])
			start = i + 1
		}
	}
	return result
}

func TestExecutionPaperKeepsInFlightCandidateAcrossFrames(t *testing.T) {
	store, pools := poolFixture(t)
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Start: pools[0].Token0, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}, Input: big.NewInt(100), Net: big.NewInt(5)}
	p := &executionPaper{jobs: make(chan paperExecutionJob, 1), policy: executionPaperPolicy{IntervalMS: 1000, WrappedNative: pools[0].Token0.Hex()}}
	r := &runtime{executionPaper: p, store: store, block: rpc.ExecutionBlock{Ref: store.Block()}}
	first := arb.SearchEvent{Stage: arb.QuoteOptimized, Job: arb.SearchJob{Version: domain.StateVersion{Source: domain.SourceStandard, Anchor: store.Block(), Epoch: 1}, View: store.View()}, Result: arb.SearchResult{Opportunities: []arb.Opportunity{opportunity}}}
	if err := r.offerExecutionPaper(first); err != nil {
		t.Fatal(err)
	}
	p.next = time.Time{}
	next := first
	next.Job.Version.Epoch = 2
	if err := r.offerExecutionPaper(next); err != nil {
		t.Fatal(err)
	}
	if len(p.jobs) != 1 || !p.busy.Load() || p.skipped.Load() != 1 {
		t.Fatal("unbounded or replaced in-flight job")
	}
	got := <-p.jobs
	if got.event.Job.Version != first.Job.Version {
		t.Fatal("next frame replaced sampled state")
	}
}

func TestExecutionPaperShutdownPersistsAdmittedSample(t *testing.T) {
	path := filepath.Join(t.TempDir(), "execution.jsonl")
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	f, err := openPaperJournal(path, chain, domain.Hash{1})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	p := &executionPaper{chain: chain, policy: executionPaperPolicy{TimeoutMS: 100}, jobs: make(chan paperExecutionJob, 1), done: make(chan struct{}), cancel: cancel}
	p.jobs <- paperExecutionJob{unavailable: "retained unavailable prefix", discovered: time.Now()}
	cancel()
	go p.run(ctx, f)
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := splitPaperLines(raw)
	if len(lines) != 2 {
		t.Fatal("admitted sample disappeared during shutdown", string(raw))
	}
	var result paperExecutionResult
	if err := json.Unmarshal(lines[1], &result); err != nil || result.Status != "unavailable" || result.StateBasis != "unverified" || result.Economics == nil || result.Economics.Status != pipeline.NetUnknown {
		t.Fatal(result, err)
	}
}

func TestExecutionPaperBackendAdmission(t *testing.T) {
	cfg := config.Config{NetworkName: "optimism", PoolConfig: "pools.json", SearchConfig: "search.json", RPCSearchRefresh: "logs", ExecutionPaperConfig: "paper.json"}
	if _, err := (Factory{}).Create(t.Context(), cfg); err != nil {
		t.Fatal("log paper simulation rejected", err)
	}
	cfg.ExecutionConfig = "dispatch.json"
	if _, err := (Factory{}).Create(t.Context(), cfg); err == nil {
		t.Fatal("virtual executor escaped into dispatch confirmation")
	}
	cfg.ExecutionConfig = ""
	cfg.NetworkName = "arbitrum-one"
	if _, err := (Factory{}).Create(t.Context(), cfg); err == nil {
		t.Fatal("OP fee semantics enabled on Nitro")
	}
}
