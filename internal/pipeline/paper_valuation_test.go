package pipeline

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestNativeValueUsesCurrentOutsideRoutePricesAndRoundsDown(t *testing.T) {
	wrapped, asset := domain.Address{1}, domain.Address{2}
	p := pool.State{Key: domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}, Token0: wrapped, Token1: asset, Reserve0: big.NewInt(2), Reserve1: big.NewInt(3)}
	store := state.New()
	if err := store.Upsert(p); err != nil {
		t.Fatal(err)
	}
	book := newPaperBook(PaperCostPolicy{WrappedNative: wrapped.Hex()})
	book.prepareConversions(store.Pools())
	r := &Research{book: book}
	cycle := graph.Cycle{Start: asset}
	value, basis, keys := r.NativeValue(store.View(), cycle, wrapped, big.NewInt(5))
	if value.Int64() != 3 || basis != "local_pool_spot_scenario" || len(keys) != 1 {
		t.Fatal(value, basis, keys)
	}
	p.Reserve1 = big.NewInt(6)
	if err := store.Upsert(p); err != nil {
		t.Fatal(err)
	}
	value, _, _ = r.NativeValue(store.View(), cycle, wrapped, big.NewInt(5))
	if value.Int64() != 1 {
		t.Fatal("stale conversion", value)
	}
	value, _, _ = r.NativeValue(store.View(), graph.Cycle{Start: domain.Address{99}}, wrapped, big.NewInt(5))
	if value != nil {
		t.Fatal("missing currency invented")
	}
	value, _, _ = r.NativeValue(nil, graph.Cycle{Start: domain.Address{}}, wrapped, big.NewInt(5))
	if value.Int64() != 5 {
		t.Fatal("V4 native currency conversion failed")
	}
}
func TestNativeValueUsesRouteTailWithoutRoundingEachHop(t *testing.T) {
	wrapped, asset := domain.Address{1}, domain.Address{2}
	key := domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}
	store := state.New()
	if err := store.Upsert(pool.State{Key: key, Token0: wrapped, Token1: asset, Reserve0: big.NewInt(2), Reserve1: big.NewInt(3)}); err != nil {
		t.Fatal(err)
	}
	var r *Research
	value, basis, _ := r.NativeValue(store.View(), graph.Cycle{Start: asset, Pools: []domain.PoolKey{key, key}}, wrapped, big.NewInt(5))
	if value.Int64() != 3 || basis != "route_spot_scenario" {
		t.Fatal(value, basis)
	}
}

// A chain run without a paper policy has no conversion book. Without spot
// conversions every USDC or USDT cycle that does not pass through WETH would
// come back "unavailable" and the reactive runtime would drop it as
// native_value_unknown, profitable cycles included.
func TestNativeValueWithoutAPaperBookUsesSpotConversions(t *testing.T) {
	wrapped, asset, other := domain.Address{1}, domain.Address{2}, domain.Address{4}
	store := state.New()
	for _, p := range []pool.State{
		{Key: domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}, Token0: wrapped, Token1: asset, Reserve0: big.NewInt(2), Reserve1: big.NewInt(3)},
		{Key: domain.PoolKey{Address: domain.Address{5}, Kind: domain.PoolKindV2}, Token0: asset, Token1: other, Reserve0: big.NewInt(700), Reserve1: big.NewInt(900)},
	} {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	r := &Research{}
	cycle := graph.Cycle{Start: asset, Pools: []domain.PoolKey{{Address: domain.Address{5}, Kind: domain.PoolKindV2}, {Address: domain.Address{5}, Kind: domain.PoolKindV2}}}
	if value, _, _ := r.NativeValue(store.View(), cycle, wrapped, big.NewInt(5)); value != nil {
		t.Fatal("valued before any block was observed")
	}
	r.SetWrappedNative(wrapped)
	r.ObserveReceipts(store.View(), domain.BlockRef{Number: 10}, nil)
	value, basis, keys := r.NativeValue(store.View(), cycle, wrapped, big.NewInt(5))
	if value == nil || value.Int64() != 3 || basis != "local_pool_spot_scenario" || len(keys) != 1 {
		t.Fatal("cycle off the wrapped native not valued", value, basis, keys)
	}
	// The pricing pool is drained before the paths are rebuilt: its price is
	// cheap to move, so the cached path is refused.
	if err := store.Upsert(pool.State{Key: domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}, Token0: wrapped, Token1: asset, Reserve0: big.NewInt(200), Reserve1: big.NewInt(300)}); err != nil {
		t.Fatal(err)
	}
	r.spot.paths = nil
	r.ObserveReceipts(store.View(), domain.BlockRef{Number: 11}, nil)
	if value, _, _ := r.NativeValue(store.View(), cycle, wrapped, big.NewInt(500)); value == nil {
		t.Fatal("deep pool not valued")
	}
	if err := store.Upsert(pool.State{Key: domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}, Token0: wrapped, Token1: asset, Reserve0: big.NewInt(80), Reserve1: big.NewInt(120)}); err != nil {
		t.Fatal(err)
	}
	if value, _, _ := r.NativeValue(store.View(), cycle, wrapped, big.NewInt(500)); value != nil {
		t.Fatal("a path whose pool lost most of its reserve still priced the asset", value)
	}
}

// A USDC cycle that passes through WETH is valued by the pools after WETH, and
// when that tail is the MetricOmm WETH/USDC pool it has no spot price: the
// pool has neither reserves nor a sqrt price. Its price is
// the oracle quote; without a quote the cycle falls back to the start
// asset's own conversion path instead of being dropped.
func TestNativeValueThroughAMetricOmmPool(t *testing.T) {
	wrapped, asset := domain.Address{1}, domain.Address{2}
	v2 := domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}
	metric := domain.PoolKey{Address: domain.Address{0x2b}, Kind: domain.PoolKindMetricOmm}
	// 2,000 quote units per whole base token; the base has 18 decimals and
	// the quote 6, so one raw quote unit is worth 5e8 raw base units.
	quote := func(whole int64) *big.Int { return new(big.Int).Lsh(big.NewInt(whole), 64) }
	state0 := pool.State{Key: metric, Token0: wrapped, Token1: asset, Metric: &pool.MetricState{
		Token0Scale: big.NewInt(1), Token1Scale: big.NewInt(1_000_000_000_000), Words: []domain.Hash{{}, {}, {}},
		BidX64: quote(1999), AskX64: quote(2001)}}
	store := state.New()
	for _, p := range []pool.State{
		{Key: v2, Token0: wrapped, Token1: asset, Reserve0: big.NewInt(1_000_000_000_000_000_000), Reserve1: big.NewInt(2_100_000_000)},
		state0,
	} {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	r := &Research{}
	r.SetWrappedNative(wrapped)
	r.ObserveReceipts(store.View(), domain.BlockRef{Number: 10}, nil)
	// asset -> wrapped on the pair, wrapped -> asset on the MetricOmm pool.
	cycle := graph.Cycle{Start: asset, Pools: []domain.PoolKey{v2, metric}}
	value, basis, _ := r.NativeValue(store.View(), cycle, wrapped, big.NewInt(4_000_000))
	if value == nil || basis != "route_spot_scenario" {
		t.Fatal("a cycle ending on a MetricOmm pool was not valued", value, basis)
	}
	// 4 quote tokens at the 2,000 mid are 0.002 base tokens.
	if want := big.NewInt(2_000_000_000_000_000); value.Cmp(want) != 0 {
		t.Fatalf("valued at %s, want %s", value, want)
	}
	// Without a quote the pair's path prices the asset: 4e6 * 1e18 / 2.1e9.
	unquoted := state0.Copy()
	unquoted.Metric.BidX64, unquoted.Metric.AskX64 = nil, nil
	if err := store.Upsert(unquoted); err != nil {
		t.Fatal(err)
	}
	value, basis, _ = r.NativeValue(store.View(), cycle, wrapped, big.NewInt(4_000_000))
	if value == nil || basis != "local_pool_spot_scenario" || value.Cmp(big.NewInt(1_904_761_904_761_904)) != 0 {
		t.Fatal("no fallback to the asset's own conversion path", value, basis)
	}
}
