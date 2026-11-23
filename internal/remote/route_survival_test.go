package remote

import (
	"math"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func TestRouteSurvivalLearnsFromFinalPaperScenarios(t *testing.T) {
	s := newRouteSurvival()
	if s.probability(2) >= 0.1 || s.probability(5) <= 0.8 {
		t.Fatal("prior does not reflect the measured split", s.probability(2), s.probability(5))
	}
	// Twenty two-hop groups that all survive pull the estimate up; an earlier
	// delay, a failed baseline or an unknown group never counts.
	for i := range 20 {
		group := domain.Hash{byte(i + 1)}
		s.observe(group, 2, false, 0, true)
		s.observe(group, 2, false, 200, false)
		s.observe(group, 2, true, 600, true)
	}
	s.observe(domain.Hash{0xee}, 2, false, 0, false)
	s.observe(domain.Hash{0xee}, 2, true, 600, true)
	s.observe(domain.Hash{0xef}, 2, true, 600, true)
	if got := s.snapshot()[2]; got != [2]int{20, 20} {
		t.Fatal("survival counted the wrong scenarios", got)
	}
	if p := s.probability(2); p < 0.5 {
		t.Fatal("observed survival did not outweigh the prior", p)
	}
}

// Production runs without execution paper: nothing observes outcomes, and the
// measured prior must still weight ranking instead of switching it off.
func TestRouteSurvivalWithoutPaperKeepsPriors(t *testing.T) {
	var none *routeSurvival
	for _, hops := range []int{2, 3, 4, 6} {
		if none.probability(hops) != survivalPriorRate(hops) || math.Abs(newRouteSurvival().probability(hops)-survivalPriorRate(hops)) > 1e-12 {
			t.Fatal("survival without paper outcomes differs from the prior", hops, none.probability(hops))
		}
	}
	if none.snapshot() != nil {
		t.Fatal("an absent estimator reported outcomes")
	}
	route := func(hops int, gross int64) arb.Opportunity {
		return arb.Opportunity{Cycle: graph.Cycle{Pools: make([]domain.PoolKey, hops), Len: hops}, Net: big.NewInt(gross)}
	}
	ranked := rankBySurvival([]arb.Opportunity{route(2, 1000), route(5, 200)}, none, func(o arb.Opportunity) *big.Int { return o.Net })
	if len(ranked[0].Cycle.Pools) != 5 {
		t.Fatal("without paper the prior no longer demoted a short route")
	}
}

func TestRankingPrefersRoutesThatSurviveOurLatency(t *testing.T) {
	route := func(hops int, gross int64) arb.Opportunity {
		return arb.Opportunity{Cycle: graph.Cycle{Pools: make([]domain.PoolKey, hops), Len: hops}, Net: big.NewInt(gross)}
	}
	short, long := route(2, 1000), route(5, 200)
	native := func(o arb.Opportunity) *big.Int { return o.Net }
	ranked := rankBySurvival([]arb.Opportunity{short, long}, newRouteSurvival(), native)
	if len(ranked[0].Cycle.Pools) != 5 {
		t.Fatal("a fivefold gross on a short route outranked a route that survives")
	}
}

// Block 159027597: 235,268 USDC units (about $0.24) against 0.111 OP (about
// $0.01). Raw start-asset amounts differ by eleven orders of magnitude in the
// wrong direction, so ranking must compare native value.
func TestRankingComparesGrossAcrossStartAssetsInNative(t *testing.T) {
	usdc, op := domain.Address{1}, domain.Address{2}
	route := func(start domain.Address, gross string) arb.Opportunity {
		net, _ := new(big.Int).SetString(gross, 10)
		return arb.Opportunity{Cycle: graph.Cycle{Start: start, Pools: make([]domain.PoolKey, 2), Len: 2}, Net: net}
	}
	// Native wei per 1e18 raw units, from the intents' recorded conversions.
	perUnit := map[domain.Address]string{usdc: "444612399383677507402703043", op: "48787199108116"}
	native := func(o arb.Opportunity) *big.Int {
		rate, ok := new(big.Int).SetString(perUnit[o.Cycle.Start], 10)
		if !ok {
			return nil
		}
		value := new(big.Int).Mul(o.Net, rate)
		return value.Quo(value, big.NewInt(1e18))
	}
	unvalued := route(domain.Address{3}, "999999999999999999999")
	ranked := rankBySurvival([]arb.Opportunity{unvalued, route(op, "111185319566479023"), route(usdc, "235268")}, newRouteSurvival(), native)
	if ranked[0].Cycle.Start != usdc || ranked[1].Cycle.Start != op || ranked[2].Cycle.Start != unvalued.Cycle.Start {
		t.Fatal("ranking compared raw amounts across assets", ranked[0].Cycle.Start, ranked[1].Cycle.Start, ranked[2].Cycle.Start)
	}
}
