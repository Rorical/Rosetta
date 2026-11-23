package remote

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// A route whose limit-ratio estimate is 312k may use 236k. A route's
// own successes price its gas; until it has three, the median of routes with
// the same kinds and funding does, then the limit ratio.
func TestRouteExpectedGasPrefersTheRoutesOwnHistory(t *testing.T) {
	e := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	route := []domain.PoolKey{{Kind: domain.PoolKindSolidlyV3, Address: domain.Address{1}}, {Kind: domain.PoolKindV4, Address: domain.Address{2}, PoolID: domain.Hash{3}}}
	sibling := []domain.PoolKey{{Kind: domain.PoolKindSolidlyV3, Address: domain.Address{4}}, {Kind: domain.PoolKindV4, Address: domain.Address{2}, PoolID: domain.Hash{5}}}
	const limit = 580_000
	if got, want := e.routeExpectedGas(route, execution.FundingV3FlashSwap, limit), e.expectedGas(limit); got != want {
		t.Fatalf("no history: %d, want the limit ratio %d", got, want)
	}
	for _, used := range []uint64{300_000, 310_000, 320_000} {
		e.observeRouteGas(sibling, execution.FundingV3FlashSwap, used)
	}
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, limit); got != 310_000 {
		t.Fatalf("same kinds: %d", got)
	}
	if got, want := e.routeExpectedGas(route, execution.FundingV3Flash, limit), e.expectedGas(limit); got != want {
		t.Fatalf("other funding shares no history: %d, want %d", got, want)
	}
	for _, used := range []uint64{230_000, 236_000, 240_000, 233_000} {
		e.observeRouteGas(route, execution.FundingV3FlashSwap, used)
	}
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, limit); got != 240_000 {
		t.Fatalf("own history p75: %d", got)
	}
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, 200_000); got != 200_000 {
		t.Fatalf("not capped at the limit: %d", got)
	}
}

// Six two-hop V3 successes used 205k–256k gas and two other routes of the
// same kinds 457k and 479k. The 75th percentile of the kinds would price
// every unmeasured two-hop route at 457k, and routes rivals take at
// 209k–246k would be dropped as below network cost.
func TestRouteGasOfUnmeasuredRoutesIsTheMedianOfTheirKinds(t *testing.T) {
	e := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	for i, used := range []uint64{478_559, 456_686, 208_327, 255_754, 241_683, 205_538} {
		measured := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{byte(10 + i)}}, {Kind: domain.PoolKindV3, Address: domain.Address{byte(20 + i)}}}
		e.observeRouteGas(measured, execution.FundingV3FlashSwap, used)
	}
	unmeasured := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{1}}, {Kind: domain.PoolKindV3, Address: domain.Address{2}}}
	if got := e.routeExpectedGas(unmeasured, execution.FundingV3FlashSwap, 750_000); got != 255_754 {
		t.Fatalf("unmeasured two-hop route priced at %d gas", got)
	}
}

// A history kept in memory only would make every restart price routes by the
// limit ratio again (about 450k for two hops) until they settled anew.
func TestRouteGasSurvivesARestart(t *testing.T) {
	e := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	route := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{1}}, {Kind: domain.PoolKindV4, Address: domain.Address{2}, PoolID: domain.Hash{3}}}
	sibling := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{4}}, {Kind: domain.PoolKindV4, Address: domain.Address{2}, PoolID: domain.Hash{5}}}
	for _, used := range []uint64{230_000, 236_000, 240_000} {
		e.observeRouteGas(route, execution.FundingV3FlashSwap, used)
	}
	now := time.UnixMilli(1_794_000_000_000)
	path := bidStorePath(filepath.Join(t.TempDir(), "reactive.jsonl"))
	if err := e.save(path, now); err != nil {
		t.Fatal(err)
	}
	restarted := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	restarted.load(path, now.Add(time.Minute))
	if got := restarted.routeExpectedGas(route, execution.FundingV3FlashSwap, 750_000); got != 240_000 {
		t.Fatalf("own history after a restart: %d", got)
	}
	if got := restarted.routeExpectedGas(sibling, execution.FundingV3FlashSwap, 750_000); got != 236_000 {
		t.Fatalf("kind history after a restart: %d", got)
	}
}

// A three-hop V3 route priced at 342,136 gas, the median of its kinds, uses
// 485,461 and wins at a small loss (the fee exceeds the gross). Routes of the same kinds differ by a
// factor of two, so a route's own measurement beats its kinds' from the first
// sample; with fewer than three, the largest is taken.
func TestRouteGasUsesTheRoutesFirstMeasurement(t *testing.T) {
	e := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	for i, used := range []uint64{300_000, 342_136, 350_000} {
		other := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{byte(10 + i)}}, {Kind: domain.PoolKindV3, Address: domain.Address{byte(20 + i)}}, {Kind: domain.PoolKindV3, Address: domain.Address{byte(30 + i)}}}
		e.observeRouteGas(other, execution.FundingV3FlashSwap, used)
	}
	route := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{1}}, {Kind: domain.PoolKindV3, Address: domain.Address{2}}, {Kind: domain.PoolKindV3, Address: domain.Address{3}}}
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, 1_000_000); got != 342_136 {
		t.Fatalf("unmeasured route: %d, want the median of its kinds", got)
	}
	e.observeRouteGas(route, execution.FundingV3FlashSwap, 485_461)
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, 1_000_000); got != 485_461 {
		t.Fatalf("after one measurement of the route itself: %d", got)
	}
	e.observeRouteGas(route, execution.FundingV3FlashSwap, 470_000)
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, 1_000_000); got != 485_461 {
		t.Fatalf("after two measurements: %d, want the larger", got)
	}
}

// With no settled route through a MetricOmm pool, each would be priced at the
// limit ratio: 450,000 gas for two hops, where rivals use a median of about
// 355,000 on such routes, leaving that much less surplus for the tip. A
// MetricOmm swap with its extensions and oracle costs 220,508 gas in a traced
// route against about 120,000 for a V3 swap, so an unmeasured route with
// MetricOmm legs is priced as the same route over V3 pools plus 100,000 gas
// a leg.
func TestUnmeasuredMetricOmmRoutesArePricedFromTheirV3Shape(t *testing.T) {
	e := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	for i, used := range []uint64{208_327, 241_683, 255_754} {
		measured := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{byte(10 + i)}}, {Kind: domain.PoolKindV3, Address: domain.Address{byte(20 + i)}}}
		e.observeRouteGas(measured, execution.FundingV3FlashSwap, used)
	}
	through := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{1}}, {Kind: domain.PoolKindMetricOmm, Address: domain.Address{2}}}
	if got := e.routeExpectedGas(through, execution.FundingV3FlashSwap, 750_000); got != 341_683 {
		t.Fatalf("an unmeasured V3 + MetricOmm route priced at %d gas", got)
	}
	// Its own measurement, and its own kinds once measured, come first.
	e.observeRouteGas(through, execution.FundingV3FlashSwap, 360_000)
	if got := e.routeExpectedGas(through, execution.FundingV3FlashSwap, 750_000); got != 360_000 {
		t.Fatalf("a measured route priced at %d gas", got)
	}
	// Without V3 history either, the limit ratio stands.
	other := []domain.PoolKey{{Kind: domain.PoolKindSlipstream, Address: domain.Address{3}}, {Kind: domain.PoolKindMetricOmm, Address: domain.Address{2}}}
	if got, want := e.routeExpectedGas(other, execution.FundingV3FlashSwap, 750_000), e.expectedGas(750_000); got != want {
		t.Fatalf("no history at all: %d, want the limit ratio %d", got, want)
	}
}
