package pipeline

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/ingest"
)

func TestPaperPersistenceReanchorsAndTruncatesOrphans(t *testing.T) {
	store, topology, cycle, version := researchFixture(t)
	policy := PaperCostPolicy{SamplePath: filepath.Join(t.TempDir(), "samples.json"), WrappedNative: cycle.Start.Hex(), HistoryBlocks: 128, QuoteHistoryBlocks: 4096, MinSamples: 1, LoanFixed: "0", MinNetProfit: "0"}
	chain := domain.ChainIdentity{ChainID: 10, Family: domain.FamilyOPStack}
	b := newPaperBook(policy)
	head := version.Anchor
	tx := ingest.Transaction{Hash: domain.Hash{44}, Successful: true, Fees: &domain.ReceiptFees{Total: big.NewInt(25)}, Logs: []ingest.Log{{Address: cycle.Pools[0].Address, Topics: []domain.Hash{auditSwapV2}}}}
	b.observeReceipts(store.View(), head, []ingest.Transaction{tx})
	audit := SwapAuditResult{Comparisons: []SwapComparison{{Pool: cycle.Pools[0], TokenIn: cycle.Start, Transaction: tx.Hash, Quoted: big.NewInt(100), Observed: big.NewInt(100), Delta: new(big.Int)}}}
	b.observeQuotes(head, audit)
	b.observeQuotes(head, audit)
	if len(b.quotes[quoteDirection{cycle.Pools[0], cycle.Start}]) != 1 {
		t.Fatal("duplicate audit counted twice")
	}
	next := domain.BlockRef{Number: head.Number + 1, Hash: domain.Hash{11}, ParentHash: head.Hash}
	tx.Hash = domain.Hash{45}
	b.observeReceipts(store.View(), next, []ingest.Transaction{tx})
	audit.Comparisons[0].Transaction = tx.Hash
	b.observeQuotes(next, audit)
	revision, err := b.persist(chain, 0)
	if err != nil || revision == 0 {
		t.Fatal(revision, err)
	}
	restored := newPaperBook(policy)
	if err = restored.restore(chain); err != nil {
		t.Fatal(err)
	}
	quote, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	if got := restored.economics(store.View(), version, quote); got.Status != NetUnknown || got.Calibration.Reason != "awaiting_canonical_cost_samples" {
		t.Fatal("unverified disk samples used", got)
	}
	// Pool checkpoint survived at the parent, while the sample file got ahead.
	restored.observeReceipts(store.View(), head, nil)
	if restored.pending || len(restored.paid) != 1 || len(restored.quotes[quoteDirection{cycle.Pools[0], cycle.Start}]) != 1 {
		t.Fatal("unpublished suffix survived restart reanchor")
	}
	fork := domain.BlockRef{Number: next.Number, Hash: domain.Hash{99}, ParentHash: head.Hash}
	restored.observeReceipts(store.View(), fork, nil)
	if len(restored.paid) != 1 || restored.headers[fork.Number] != fork {
		t.Fatal("fork lost common-prefix evidence")
	}
	if _, err = restored.persist(chain, 0); err != nil {
		t.Fatal(err)
	}
	again := newPaperBook(policy)
	if err = again.restore(chain); err != nil {
		t.Fatal(err)
	}
	again.observeReceipts(store.View(), fork, nil)
	if again.pending || len(again.paid) != 1 {
		t.Fatal("second restart lost evidence")
	}
	if err = newPaperBook(policy).restore(domain.ChainIdentity{ChainID: 42161, Family: domain.FamilyNitro}); err == nil {
		t.Fatal("cross-chain sample file accepted")
	}
	if err = os.WriteFile(policy.SamplePath, []byte(`{"version":`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = newPaperBook(policy).restore(chain); err == nil {
		t.Fatal("truncated sample file accepted")
	}
}

func TestPaperSeparateQuoteRetentionAndUnknownFork(t *testing.T) {
	store, _, cycle, v := researchFixture(t)
	b := newPaperBook(PaperCostPolicy{HistoryBlocks: 1, QuoteHistoryBlocks: 4})
	b.observeReceipts(store.View(), v.Anchor, nil)
	b.observeQuotes(v.Anchor, SwapAuditResult{Comparisons: []SwapComparison{{Pool: cycle.Pools[0], TokenIn: cycle.Start, Quoted: big.NewInt(1), Observed: big.NewInt(1), Delta: new(big.Int)}}})
	for i := uint64(1); i <= 3; i++ {
		b.observeReceipts(store.View(), domain.BlockRef{Number: v.Anchor.Number + i, Hash: domain.Hash{byte(10 + i)}, ParentHash: b.head.Hash}, nil)
	}
	if len(b.quotes) != 1 {
		t.Fatal("quote history incorrectly expired with fee history")
	}
	b.observeReceipts(store.View(), domain.BlockRef{Number: 20, Hash: domain.Hash{20}, ParentHash: domain.Hash{99}}, nil)
	if len(b.quotes) != 0 {
		t.Fatal("detached history retained")
	}
}

func TestResearchClosesAndReloadsPersistentSamples(t *testing.T) {
	store, _, cycle, version := researchFixture(t)
	dir := t.TempDir()
	policy := ResearchPolicy{MaxTracked: 8, MaxAgeMS: 1000, EvaluationMS: 20, Paper: &PaperCostPolicy{
		SamplePath: filepath.Join(dir, "samples.json"), WrappedNative: cycle.Start.Hex(), HistoryBlocks: 128, MinSamples: 1, FundingBasis: "own_capital_scenario", LoanFixed: "0", MinNetProfit: "0",
	}}
	encoded, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	config := filepath.Join(dir, "policy.json")
	if err = os.WriteFile(config, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	chain := domain.ChainIdentity{ChainID: 10, Family: domain.FamilyOPStack}
	first, err := NewResearch(t.Context(), config, filepath.Join(dir, "first.jsonl"), chain)
	if err != nil {
		t.Fatal(err)
	}
	first.ObserveReceipts(store.View(), version.Anchor, []ingest.Transaction{{Hash: domain.Hash{44}, Successful: true, Fees: &domain.ReceiptFees{Total: big.NewInt(25)}, Logs: []ingest.Log{{Address: cycle.Pools[0].Address, Topics: []domain.Hash{auditSwapV2}}}}})
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := NewResearch(t.Context(), config, filepath.Join(dir, "second.jsonl"), chain)
	if err != nil {
		t.Fatal(err)
	}
	second.ObserveReceipts(store.View(), version.Anchor, nil)
	if second.book.pending || len(second.book.paid) != 1 {
		t.Fatal("restart discarded saved fees")
	}
	if err = second.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPaperPersistenceRestoresV4NativeDirection(t *testing.T) {
	store, _, cycle, version := researchFixture(t)
	policy := PaperCostPolicy{SamplePath: filepath.Join(t.TempDir(), "samples.json"), WrappedNative: cycle.Start.Hex(), HistoryBlocks: 128, QuoteHistoryBlocks: 4096}
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	for _, kind := range []domain.PoolKind{domain.PoolKindV4, domain.PoolKindV2} {
		b := newPaperBook(policy)
		b.observeReceipts(store.View(), version.Anchor, nil)
		key := domain.PoolKey{Kind: kind, Address: domain.Address{1}, PoolID: domain.Hash{2}}
		b.observeQuotes(version.Anchor, SwapAuditResult{Comparisons: []SwapComparison{{Pool: key, TokenIn: domain.Address{}, Transaction: domain.Hash{3}, Quoted: big.NewInt(100), Observed: big.NewInt(100), Delta: new(big.Int)}}})
		if _, err := b.persist(chain, 0); err != nil {
			t.Fatal(err)
		}
		restored := newPaperBook(policy)
		err := restored.restore(chain)
		if kind == domain.PoolKindV4 {
			if err != nil {
				t.Fatal("valid native currency rejected", err)
			}
			if len(restored.quotes[quoteDirection{Pool: key}]) != 1 {
				t.Fatal("native sample lost")
			}
		} else if err == nil {
			t.Fatal("zero ERC20 address accepted for V2")
		}
	}
}

// A paid sample from a single fee receipt carries only the block's number and
// hash; saved as is, the book would fail its own restore check and the bot
// could not start. Samples take the book's header for their block.
func TestPaidExecutionWithAPartialBlockRefRestores(t *testing.T) {
	_, _, cycle, version := researchFixture(t)
	policy := PaperCostPolicy{SamplePath: filepath.Join(t.TempDir(), "samples.json"), WrappedNative: cycle.Start.Hex(), HistoryBlocks: 128, QuoteHistoryBlocks: 4096, MinSamples: 1, LoanFixed: "0", MinNetProfit: "0"}
	chain := domain.ChainIdentity{ChainID: 10, Family: domain.FamilyOPStack}
	b := newPaperBook(policy)
	head := version.Anchor
	head.ParentHash, head.Timestamp = domain.Hash{9}, 1_793_718_769
	b.headers[head.Number], b.head = head, head
	steps := []execution.ExecutorStep{{Pool: cycle.Pools[0]}, {Pool: cycle.Pools[1]}}
	settled := &execution.Settlement{Hash: domain.Hash{46}, Success: true, Block: domain.BlockRef{Number: head.Number, Hash: head.Hash}, Fees: &domain.ReceiptFees{Total: big.NewInt(25)}, Intent: &execution.ReactiveIntent{Route: execution.ExecutorPlan{Steps: steps}}}
	(&Research{book: b}).RecordPaidExecution(settled)
	if len(b.paid) != 1 || b.paid[0].block != head {
		t.Fatal("paid sample did not take the book's header", b.paid)
	}
	if _, err := b.persist(chain, 0); err != nil {
		t.Fatal(err)
	}
	if err := newPaperBook(policy).restore(chain); err != nil {
		t.Fatal("saved book does not restore:", err)
	}
	// A file written before the fix holds the partial reference.
	b.paid[0].block = settled.Block
	if _, err := b.persist(chain, 0); err != nil {
		t.Fatal(err)
	}
	old := newPaperBook(policy)
	if err := old.restore(chain); err != nil || len(old.paid) != 1 || old.paid[0].block != head {
		t.Fatal("old file with a partial paid block", err)
	}
}
