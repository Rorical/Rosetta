package pipeline

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestProtocolEnvelopeCoversNewMixturesAndKeepsExactPreference(t *testing.T) {
	b := newPaperBook(PaperCostPolicy{MinSamples: 3})
	v2 := domain.PoolKey{Kind: domain.PoolKindV2}
	v3 := domain.PoolKey{Kind: domain.PoolKindV3}
	add := func(keys []domain.PoolKey, fee int64, seed byte) {
		for i := range 3 {
			b.paid = append(b.paid, paidSample{shape: paperShape(keys), tx: domain.Hash{seed, byte(i)}, fees: &domain.ReceiptFees{Total: big.NewInt(fee)}})
		}
	}
	add([]domain.PoolKey{v2, v2}, 101, 1)
	add([]domain.PoolKey{v3}, 70, 2)
	keys := []domain.PoolKey{v2, v3, v3, v3}
	var evidence PaperCalibration
	if !b.estimateCost(keys, &evidence) || evidence.CostModel != "protocol_hop_envelope" || evidence.NativeNetworkFee.Int64() != 261 || evidence.FeeSamples != 3 {
		t.Fatalf("mixed shape: %+v", evidence)
	}
	add(keys, 200, 3)
	evidence = PaperCalibration{}
	if !b.estimateCost(keys, &evidence) || evidence.CostModel != "matching_transaction_shape" || evidence.NativeNetworkFee.Int64() != 200 {
		t.Fatalf("exact shape: %+v", evidence)
	}
	evidence = PaperCalibration{}
	if b.estimateCost([]domain.PoolKey{v2, {Kind: domain.PoolKindV4}}, &evidence) || evidence.NativeNetworkFee != nil {
		t.Fatal("unsampled protocol inherited another protocol's fees")
	}
}

func TestPaperNetStatusSeparatesMissingCalibrationFromLoss(t *testing.T) {
	for _, test := range []struct {
		net  *big.Int
		want EconomicsStatus
	}{
		{nil, NetUnknown}, {big.NewInt(-1), NetNonPositive}, {big.NewInt(0), NetNonPositive}, {big.NewInt(1), NetBelowMinimum}, {big.NewInt(2), NetEligible},
	} {
		result := ResearchEconomics{Net: test.net}
		result.classify(big.NewInt(2))
		if result.Status != test.want || result.Eligible != (test.want == NetEligible) {
			t.Fatal(result)
		}
	}
	store, topology, cycle, version := researchFixture(t)
	b := newPaperBook(PaperCostPolicy{WrappedNative: cycle.Start.Hex(), HistoryBlocks: 128, MinSamples: 1, LoanFixed: "0", MinNetProfit: "0"})
	b.head = version.Anchor
	b.headers[version.Anchor.Number] = version.Anchor
	b.paid = []paidSample{{shape: paperShape(cycle.Pools), fees: &domain.ReceiptFees{Total: big.NewInt(1000000)}}}
	quote, err := topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(100))
	if err != nil {
		t.Fatal(err)
	}
	result := b.economics(store.View(), version, quote)
	if result.Net == nil || result.Net.Sign() >= 0 || result.Status != NetUnknown || result.Calibration.Reason != "insufficient_direction_calibration" {
		t.Fatalf("partial negative estimate incorrectly finalized: %+v", result)
	}
	p1, _ := store.Pool(cycle.Pools[1])
	b.observeQuotes(version.Anchor, SwapAuditResult{Comparisons: []SwapComparison{
		{Pool: cycle.Pools[0], TokenIn: cycle.Start, Quoted: big.NewInt(100), Observed: big.NewInt(100), Delta: new(big.Int)},
		{Pool: cycle.Pools[1], TokenIn: p1.Token1, Quoted: big.NewInt(100), Observed: big.NewInt(100), Delta: new(big.Int)},
	}})
	result = b.economics(store.View(), version, quote)
	if result.Status != NetNonPositive || result.Eligible {
		t.Fatalf("complete losing estimate: %+v", result)
	}
}

func TestEconomicsCountersKeepUnknownSeparateFromNonPositive(t *testing.T) {
	var counters economicsCounters
	counters.record(arb.QuoteInitial, NetUnknown)
	counters.record(arb.QuoteOptimized, NetUnknown)
	counters.record(arb.QuoteOptimized, NetNonPositive)
	counters.record(arb.QuoteOptimized, NetBelowMinimum)
	counters.record(arb.QuoteOptimized, NetEligible)
	report := counters.snapshot().report()
	if report["initial"][NetUnknown] != 1 || report["initial"][NetNonPositive] != 0 {
		t.Fatal(report)
	}
	for _, status := range economicsStatuses {
		if report["optimized"][status] != 1 {
			t.Fatal(report)
		}
	}
}
