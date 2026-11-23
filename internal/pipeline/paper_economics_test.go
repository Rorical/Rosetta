package pipeline

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestPaperChargesObservedCostsAndDirectionalQuoteReserve(t *testing.T) {
	store, topology, cycle, version := researchFixture(t)
	policy := PaperCostPolicy{WrappedNative: cycle.Start.Hex(), HistoryBlocks: 2, MinSamples: 1, FundingBasis: "configured_loan_scenario", LoanFeePips: 10000, LoanFixed: "3", MinNetProfit: "0"}
	b := newPaperBook(policy)
	head := version.Anchor
	p0, _ := store.Pool(cycle.Pools[0])
	p1, _ := store.Pool(cycle.Pools[1])
	fees := &domain.ReceiptFees{Family: domain.FamilyOPStack, GasUsed: 10, GasPrice: big.NewInt(2), Execution: big.NewInt(20), L1: big.NewInt(4), Operator: big.NewInt(1), Total: big.NewInt(25)}
	tx := ingest.Transaction{Hash: domain.Hash{44}, Successful: true, Fees: fees, Logs: []ingest.Log{{Address: p0.Key.Address, Topics: []domain.Hash{auditSwapV2}}, {Address: p1.Key.Address, Topics: []domain.Hash{auditSwapV2}}}}
	b.observeReceipts(store.View(), head, []ingest.Transaction{tx})
	b.observeQuotes(head, SwapAuditResult{Comparisons: []SwapComparison{{Pool: p0.Key, TokenIn: cycle.Start, Quoted: big.NewInt(10000), Observed: big.NewInt(9990), Delta: big.NewInt(10)}, {Pool: p1.Key, TokenIn: p1.Token1, Quoted: big.NewInt(10000), Observed: big.NewInt(10000), Delta: big.NewInt(0)}}})
	quote, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	got := b.economics(store.View(), version, quote)
	if !got.Eligible || got.NetworkFee.String() != "25" || got.LoanFee.String() != "4" || got.Safety.String() != "1" || got.Net.Cmp(new(big.Int).Sub(quote.Net, big.NewInt(30))) != 0 || got.Calibration.ReservePips != 1000 || got.Calibration.SampleTransaction != tx.Hash {
		t.Fatalf("cost accounting %+v calibration %+v", got, got.Calibration)
	}
	// A new cold direction cannot inherit the calibrated opposite direction.
	key := quoteDirection{p0.Key, cycle.Start}
	delete(b.quotes, key)
	got = b.economics(store.View(), version, quote)
	if got.Eligible || got.Calibration.Reason != "insufficient_direction_calibration" {
		t.Fatal(got)
	}
	// Neither historical prefixes nor detached anchors may use later samples.
	old := version
	old.BlockNumber = head.Number
	got = b.economics(store.View(), old, quote)
	if got.Net != nil || got.Calibration.Reason != "cost_evidence_after_quote_position" {
		t.Fatal(got)
	}
	fork := domain.BlockRef{Number: head.Number, Hash: domain.Hash{77}}
	b.observeReceipts(store.View(), fork, nil)
	if len(b.paid) != 0 || len(b.quotes) != 0 {
		t.Fatal("reorg retained costs or calibration")
	}
	if fees.Total.String() != "25" {
		t.Fatal("cost calculation mutated receipt")
	}
}
func TestPaperNativeConversionAndUnknownStableCurve(t *testing.T) {
	p := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3}, Token0: domain.Address{1}, Token1: domain.Address{2}, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(2), 96)}
	n, d, ok := paperSpot(p, p.Token0)
	if !ok || new(big.Int).Quo(n, d).Int64() != 4 {
		t.Fatal("raw currency ratio")
	}
	p.Key.Kind = domain.PoolKindVelodromeV2
	p.Velodrome = &pool.VelodromeState{Stable: true}
	p.Reserve0 = big.NewInt(1)
	p.Reserve1 = big.NewInt(2)
	if _, _, ok := paperSpot(p, p.Token0); ok {
		t.Fatal("stable reserve ratio became a conversion price")
	}
}
func TestPaperCalibrationsExpireAndIgnoreDetachedAudits(t *testing.T) {
	store, _, cycle, v := researchFixture(t)
	b := newPaperBook(PaperCostPolicy{HistoryBlocks: 1})
	b.observeReceipts(store.View(), v.Anchor, nil)
	row := SwapComparison{Pool: cycle.Pools[0], TokenIn: cycle.Start, Quoted: big.NewInt(10), Observed: big.NewInt(9), Delta: big.NewInt(1)}
	b.observeQuotes(domain.BlockRef{Number: v.Anchor.Number, Hash: domain.Hash{99}}, SwapAuditResult{Comparisons: []SwapComparison{row}})
	if len(b.quotes) > 0 {
		t.Fatal("orphan audit applied")
	}
	b.observeQuotes(v.Anchor, SwapAuditResult{Comparisons: []SwapComparison{row}})
	next := domain.BlockRef{Number: v.Anchor.Number + 2, Hash: domain.Hash{12}, ParentHash: v.Anchor.Hash}
	b.observeReceipts(store.View(), next, nil)
	if len(b.quotes) > 0 {
		t.Fatal("stale empirical reserve persisted")
	}
}
