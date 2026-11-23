package arb

import (
	"context"
	"math/big"
	"math/rand"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func TestBoundedRankingMatchesFullSortIncludingTies(t *testing.T) {
	random := rand.New(rand.NewSource(1))
	for _, limit := range []int{1, 3, 20} {
		var reference []rankedCycle
		bounded := &rankedHeap{}
		for i := 0; i < 200; i++ {
			candidate := rankedCycle{cycle: graph.Cycle{Pools: []domain.PoolKey{{Address: domain.Address{byte(i)}}}}, net: big.NewInt(int64(random.Intn(10)))}
			reference = append(reference, candidate)
			bounded.admit(candidate, limit)
			if bounded.Len() > limit {
				t.Fatal("ranking retained more than the exact budget")
			}
		}
		slices.SortFunc(reference, compareRanked)
		actual := slices.Clone(*bounded)
		slices.SortFunc(actual, compareRanked)
		for i := range actual {
			if compareRanked(actual[i], reference[i]) != 0 {
				t.Fatal("bounded admission changed full-sort ranking", limit, i)
			}
		}
	}
}

func TestSelectionPreparesOnlyProfitableDirtyPaths(t *testing.T) {
	first := testV2State(10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(20, 2, 1, 1_000_000, 2_000_000)
	untouched := testV2State(30, 3, 4, 1_000_000, 2_000_000)
	other := testV2State(40, 4, 3, 1_000_000, 2_000_000)
	quoter := v2CurveQuoter()
	engine := mustEngine(t, mustTable(t, 2, first, second, untouched, other), 1000, 1, quoter)
	view := newFakeView().add(first).add(second).add(untouched).add(other)
	options := rankedOptions(first.Token0, 100_000)
	options.Bounds = func(domain.Address) (InputBounds, bool) {
		return InputBounds{Min: big.NewInt(1), Max: big.NewInt(100_000)}, true
	}
	selection, err := engine.Rank(t.Context(), view, []domain.PoolKey{first.Key}, options)
	if err != nil || len(quoter.calls) != 0 || len(selection.candidates) != 2 {
		t.Fatal("ranking used exact quotes or lost funding denomination", err)
	}
	if !slices.Equal(selection.Pools(), []domain.PoolKey{first.Key, second.Key}) {
		t.Fatal("untouched component reached exact preparation")
	}
	result, err := engine.Optimize(context.Background(), view, selection)
	if err != nil || len(result.Opportunities) != 2 {
		t.Fatal("selected paths did not finish exact optimization", err)
	}
}

// Admission kernel only: estimates are precomputed, excluding pool reads,
// coarse math, exact quotes, EVM confirmation and transport latency.
func BenchmarkRankedAdmission(b *testing.B) {
	random := rand.New(rand.NewSource(1))
	candidates := make([]rankedCycle, 10000)
	for i := range candidates {
		candidates[i] = rankedCycle{net: big.NewInt(random.Int63n(1_000_000)), cycle: graph.Cycle{Pools: []domain.PoolKey{{Address: domain.Address{byte(i), byte(i >> 8)}}}}}
	}
	for _, strategy := range []string{"full-sort", "bounded-8"} {
		b.Run(strategy, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if strategy == "full-sort" {
					ranked := slices.Clone(candidates)
					slices.SortFunc(ranked, compareRanked)
					if len(ranked) < 8 {
						b.Fatal("missing candidates")
					}
				} else {
					bounded := &rankedHeap{}
					for _, candidate := range candidates {
						bounded.admit(candidate, 8)
					}
					if bounded.Len() != 8 {
						b.Fatal("missing candidates")
					}
				}
			}
		})
	}
}
