package main

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func TestAttributionDoesNotAssertSupplementalCatalogAbsence(t *testing.T) {
	a := &automaticAudit{}
	c := &cycleAudit{diagnostic: &backtest.Diagnostic{Reasons: []string{"table_truncated"}}}
	v := backtest.Visibility{Status: "observable", Available: 100, Deadline: 1000}
	reason := a.missReason(c, v, "not_reported")
	row := attribution(c, v, reason, 0, 0)
	if row.Stage != "catalog_unknown" || row.Evidence != "primary_catalog_only" {
		t.Fatal(row)
	}
	c.diagnostic.Reasons = []string{"missing_pool"}
	v.Status = "no_positive_visible_prefix"
	if reason := a.missReason(c, v, "not_evaluated"); reason != "missing_pool" {
		t.Fatal("coverage failure hidden by failed quote", reason)
	}
	row = attribution(c, backtest.Visibility{Received: 50, Available: 100, Deadline: 1000}, "publication_late", 500, 1100)
	if row.StateApplyDelayNS == nil || *row.StateApplyDelayNS != 50 || row.PublicationAfterQuoteNS == nil || *row.PublicationAfterQuoteNS != 600 || *row.DeadlineSlackNS != -100 {
		t.Fatal(row)
	}
}
func TestOmissionSummaryKeepsLargestPerCurrency(t *testing.T) {
	a := &automaticAudit{}
	for i := 0; i < 30; i++ {
		c := &cycleAudit{cycle: backtest.ClosedCycle{KnownRoute: backtest.KnownRoute{Cycle: graph.Cycle{Start: domain.Address{1}}}, Transaction: domain.Hash{byte(i)}, ActualGross: big.NewInt(int64(i + 1))}}
		a.recordOmission(c, auditAttribution{Reason: "missing_pool", Stage: "coverage"}, nil)
	}
	row := a.omissions[domain.Address{1}.Hex()]
	if len(row.Largest) != 20 || row.Largest[0].Gross.Int64() != 30 || row.GrossByReason["missing_pool"].Int64() != 465 || row.Counts["missing_pool"] != 30 {
		t.Fatal(row)
	}
}

func TestAuditSeparatesStartupCatchupFromObservedMisses(t *testing.T) {
	a := &automaticAudit{block: backtest.Record{Block: domain.BlockRef{Number: 90}}, trace: traceData{frames: map[uint64][]backtest.PrefixEvidence{100: {}, 110: {}}}}
	c := &cycleAudit{}
	v := backtest.Visibility{Status: "capture_incomplete"}
	if got := a.missReason(c, v, "not_evaluated"); got != "before_feed_capture" {
		t.Fatal(got)
	}
	a.block.Block.Number = 111
	if got := a.missReason(c, v, "not_evaluated"); got != "after_feed_capture" {
		t.Fatal(got)
	}
	a.block.Block.Number = 105
	if got := a.missReason(c, v, "not_evaluated"); got != "capture_incomplete" {
		t.Fatal(got)
	}
}

func TestInitialQuoteDoesNotProveAmountOptimizationFinished(t *testing.T) {
	for _, tc := range []struct {
		stage               bool
		published, deadline int64
		want                string
	}{{false, 0, 1000, "stage_unknown"}, {true, 0, 1000, "not_observed"}, {true, 999, 1000, "reported"}, {true, 1000, 1000, "published_late"}, {true, 500, 0, "window_unknown"}} {
		if got := optimizedPublication(tc.stage, tc.published, tc.deadline); got != tc.want {
			t.Fatal(tc, got)
		}
	}
	row := attribution(&cycleAudit{}, backtest.Visibility{Status: "observable"}, "amount_optimization_after_window", 100, 120)
	if row.Stage != "amount_optimization" {
		t.Fatal(row)
	}
}
