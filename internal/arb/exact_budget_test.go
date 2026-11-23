package arb

import (
	"math/big"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func budgetCandidate(asset byte, net int64) rankedCycle {
	return rankedCycle{cycle: graph.Cycle{Start: domain.Address{asset}}, net: big.NewInt(net)}
}

func TestGrantSizingKeepsAdmissionOrderAndReservesLateLeaders(t *testing.T) {
	budgets := make(map[domain.Address]*exactBudget)
	steps := []struct {
		candidate rankedCycle
		want      bool
	}{
		{budgetCandidate(1, 10), true},
		{budgetCandidate(1, 9), true},
		{budgetCandidate(2, 1), true}, // each funding asset has its own budget
		{budgetCandidate(1, 8), true},
		{budgetCandidate(1, 7), true},
		{budgetCandidate(1, 6), false},  // the budget of four is spent
		{budgetCandidate(1, 20), true},  // a late leader uses the limit/4 reserve
		{budgetCandidate(1, 40), false}, // the reserve is spent too
	}
	for i, step := range steps {
		if got := grantSizing(budgets, step.candidate, 4); got != step.want {
			t.Fatalf("step %d: granted %v, want %v", i, got, step.want)
		}
	}
	for range 10 {
		if !grantSizing(budgets, budgetCandidate(1, 1), 0) {
			t.Fatal("zero limit must size every candidate")
		}
	}
}

func TestLimitPerAssetKeepsTopRankedOfEachAsset(t *testing.T) {
	selection := RankedSelection{candidates: []rankedCycle{
		budgetCandidate(1, 9), budgetCandidate(1, 8), budgetCandidate(1, 7),
		budgetCandidate(2, 5),
		budgetCandidate(3, 4), budgetCandidate(3, 3), budgetCandidate(3, 2),
	}, considered: 11}
	limited := selection.limitPerAsset(2)
	var nets []int64
	for _, candidate := range limited.candidates {
		nets = append(nets, candidate.net.Int64())
	}
	if !slices.Equal(nets, []int64{9, 8, 5, 4, 3}) || limited.considered != 11 {
		t.Fatal("wrong per-asset limit", nets, limited.considered)
	}
	if len(selection.limitPerAsset(0).candidates) != 7 {
		t.Fatal("zero limit must keep the selection")
	}
}

// Candidates past ExactK keep their probe and lose only refinement. Every
// optimized quote that remains is identical to the uncapped search's quote.
func TestExactKSizesOnlyBudgetedCandidates(t *testing.T) {
	fixture := newFrameFixture(t)
	_, uncapped, all := streamFrame(t, fixture, false)
	fixture.opts.ExactK = 2
	_, capped, limited := streamFrame(t, fixture, false)
	if limited.Selected != all.Selected || limited.ProbeOnly == 0 || limited.ProbeOnly >= limited.Selected {
		t.Fatal("ExactK must keep every selected probe and skip some refinements", limited.Selected, all.Selected, limited.ProbeOnly)
	}
	sized := make(map[domain.Address]int)
	for _, quote := range capped {
		sized[quote.Cycle.Start]++
		index := slices.IndexFunc(uncapped, func(o Opportunity) bool {
			return o.Cycle.Start == quote.Cycle.Start && slices.Equal(o.Cycle.Pools, quote.Cycle.Pools)
		})
		if index < 0 || uncapped[index].Input.Cmp(quote.Input) != 0 || uncapped[index].Net.Cmp(quote.Net) != 0 {
			t.Fatal("capped search changed an optimized quote", quote.Cycle, quote.Net)
		}
	}
	for asset, count := range sized {
		if count > 2+1 {
			t.Fatal("asset sized past its budget and reserve", asset, count)
		}
	}
	if len(capped) == 0 || len(capped) > len(uncapped) {
		t.Fatal("unexpected optimized count", len(capped), len(uncapped))
	}

	// Without streaming there is no probe to keep: only the top two per
	// asset are quoted at all, and their quotes are unchanged.
	fixture.opts.StreamInitial = false
	_, unstreamed, final := streamFrame(t, fixture, false)
	if final.Selected == 0 || final.Selected > 2*len(frameFunding(t)) || final.ProbeOnly != 0 {
		t.Fatal("unstreamed ExactK selection", final.Selected, final.ProbeOnly)
	}
	for _, quote := range unstreamed {
		index := slices.IndexFunc(uncapped, func(o Opportunity) bool {
			return o.Cycle.Start == quote.Cycle.Start && slices.Equal(o.Cycle.Pools, quote.Cycle.Pools)
		})
		if index < 0 || uncapped[index].Net.Cmp(quote.Net) != 0 {
			t.Fatal("unstreamed capped search changed a quote", quote.Cycle, quote.Net)
		}
	}
}
