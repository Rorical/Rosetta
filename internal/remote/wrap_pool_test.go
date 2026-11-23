package remote

import (
	"context"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/dex/wrap"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

// noReads fails a test that reads the chain.
type noReads struct{ t *testing.T }

func (c noReads) CallContract(context.Context, domain.Address, []byte, string, *string) error {
	c.t.Fatal("read the chain for a wrap pool")
	return nil
}

// The wrap pool follows the reactive policy on every start: added when it is
// missing, left alone when present, replaced when the wrapped token changed
// and removed when the policy is off, whatever a snapshot carried. It is never
// left marked as changed, and the specs a reseed reads follow the store.
func TestWrapPoolFollowsThePolicyAcrossSnapshots(t *testing.T) {
	store, pools := poolFixture(t)
	specs := specsFor(store.Pools())
	wrapped := addressAt(1)
	wrapKinds := func(specs []bootstrap.Spec) (found []domain.Address) {
		for _, spec := range specs {
			if spec.Kind == domain.PoolKindWrap {
				found = append(found, spec.Address)
			}
		}
		return found
	}

	// Off and absent: nothing changes, and the specs are the caller's own.
	same, err := setWrapPool(store, specs, domain.Address{})
	if err != nil || len(same) != len(specs) || store.Len() != len(pools) {
		t.Fatalf("off: %d specs, %d pools, %v", len(same), store.Len(), err)
	}

	specs, err = setWrapPool(store, specs, wrapped)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := store.Pool(wrap.Key(wrapped))
	if !ok || wrap.Validate(state) != nil || store.Len() != len(pools)+1 {
		t.Fatalf("wrap pool not added: %+v", state)
	}
	if len(store.Dirty()) != 0 {
		t.Fatalf("injection marked pools changed: %v", store.Dirty())
	}
	if got := wrapKinds(specs); !slices.Equal(got, []domain.Address{wrapped}) || len(specs) != len(pools)+1 {
		t.Fatalf("specs %v", got)
	}
	// A reseed rebuilds the store from the specs without reading the wrap.
	for _, spec := range specs {
		if spec.Kind != domain.PoolKindWrap {
			continue
		}
		reseeded := poolstate.New()
		if err := bootstrap.Seed(t.Context(), noReads{t}, reseeded, []bootstrap.Spec{spec}, store.Block(), 1); err != nil {
			t.Fatalf("reseed: %v", err)
		}
		if _, ok := reseeded.Pool(wrap.Key(wrapped)); !ok {
			t.Fatal("reseed lost the wrap pool")
		}
	}

	// Present: a second start changes nothing.
	again, err := setWrapPool(store, specs, wrapped)
	if err != nil || len(again) != len(specs) || store.Len() != len(pools)+1 || len(store.Dirty()) != 0 {
		t.Fatalf("second start: %d specs, %d pools, %v", len(again), store.Len(), err)
	}

	// The snapshot carries it; a start with the policy off drops it again.
	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	loaded := poolstate.New()
	if err := loaded.LoadSnapshot(data); err != nil {
		t.Fatal(err)
	}
	if _, ok := loaded.Pool(wrap.Key(wrapped)); !ok {
		t.Fatal("snapshot lost the wrap pool")
	}
	other := addressAt(2)
	moved, err := setWrapPool(loaded, specsFor(loaded.Pools()), other)
	if err != nil || !slices.Equal(wrapKinds(moved), []domain.Address{other}) || loaded.Len() != len(pools)+1 {
		t.Fatalf("another wrapped token: %v, %d pools, %v", wrapKinds(moved), loaded.Len(), err)
	}
	off, err := setWrapPool(loaded, moved, domain.Address{})
	if err != nil || len(wrapKinds(off)) != 0 || len(off) != len(pools) || loaded.Len() != len(pools) {
		t.Fatalf("off after a snapshot: %d specs, %d pools, %v", len(off), loaded.Len(), err)
	}
	for _, p := range loaded.Pools() {
		if p.Key.Kind == domain.PoolKindWrap {
			t.Fatal("wrap pool survived the policy being off")
		}
	}

	// Pools a snapshot left marked stay marked.
	if err := store.Upsert(pools[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := setWrapPool(store, specs, domain.Address{}); err != nil {
		t.Fatal(err)
	}
	if dirty := store.Dirty(); !slices.Equal(dirty, []domain.PoolKey{pools[0].Key}) {
		t.Fatalf("dirty after removal: %v", dirty)
	}
}

// wrap_native is off unless the reactive policy sets it, and without a
// reactive policy there is no wrap pool.
func TestWrapNativeIsOffByDefault(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "configs", "arbitrum-one-reactive.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	network := config.Network{Family: domain.FamilyNitro, ChainID: 42161}
	write := func(text string) config.Config {
		path := filepath.Join(t.TempDir(), "reactive.json")
		if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
		return config.Config{Network: network, ReactiveConfig: path}
	}
	if token, err := wrapNativeToken(config.Config{Network: network}); err != nil || token != (domain.Address{}) {
		t.Fatalf("no reactive policy: %s, %v", token.Hex(), err)
	}
	if !strings.Contains(string(example), `"wrap_native": false,`) {
		t.Fatal("the example does not show wrap_native off")
	}
	if token, err := wrapNativeToken(write(string(example))); err != nil || token != (domain.Address{}) {
		t.Fatalf("example: %s, %v", token.Hex(), err)
	}
	absent := strings.Replace(string(example), `"wrap_native": false,`, ``, 1)
	cfg := write(absent)
	if token, err := wrapNativeToken(cfg); err != nil || token != (domain.Address{}) {
		t.Fatalf("flag absent: %s, %v", token.Hex(), err)
	}
	settings, err := loadReactiveSettings(cfg.ReactiveConfig, domain.ChainIdentity{Family: network.Family, ChainID: network.ChainID})
	if err != nil || settings.planning.WrapNative {
		t.Fatalf("flag absent: planning admits wrap legs, %v", err)
	}
	cfg = write(strings.Replace(string(example), `"wrap_native": false,`, `"wrap_native": true,`, 1))
	token, err := wrapNativeToken(cfg)
	if err != nil || token.Hex() != "0x82af49447d8a07e3bd95bd0d56f35241523fbab1" {
		t.Fatalf("flag on: %s, %v", token.Hex(), err)
	}
	settings, err = loadReactiveSettings(cfg.ReactiveConfig, domain.ChainIdentity{Family: network.Family, ChainID: network.ChainID})
	if err != nil || !settings.planning.WrapNative || settings.wrapped != token {
		t.Fatalf("flag on: planning refuses wrap legs, %v", err)
	}
	if _, err := wrapNativeToken(write(`{`)); err == nil {
		t.Fatal("a broken policy was read as wrap_native off")
	}
}

// Until a route with a wrap leg has been measured, its gas is that of the
// same route without the leg plus the wrap prior.
func TestUnmeasuredWrapRouteAddsTheWrapPriorToItsSwaps(t *testing.T) {
	e := newBidEstimator(400*time.Millisecond, 700*time.Millisecond)
	v3 := domain.PoolKey{Kind: domain.PoolKindV3, Address: domain.Address{1}}
	v4 := domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{2}, PoolID: domain.Hash{3}}
	wrapKey := wrap.Key(domain.Address{4})
	route := []domain.PoolKey{v3, v4, wrapKey}
	const limit = 1_000_000
	if got, want := e.routeExpectedGas(route, execution.FundingV3FlashSwap, limit), e.expectedGas(limit); got != want {
		t.Fatalf("no history: %d, want the limit ratio %d", got, want)
	}
	for i, used := range []uint64{300_000, 310_000, 320_000} {
		measured := []domain.PoolKey{{Kind: domain.PoolKindV3, Address: domain.Address{byte(10 + i)}}, {Kind: domain.PoolKindV4, Address: domain.Address{2}, PoolID: domain.Hash{byte(20 + i)}}}
		e.observeRouteGas(measured, execution.FundingV3FlashSwap, used)
	}
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, limit); got != 310_000+planning.WrapLegGas {
		t.Fatalf("swaps plus the wrap prior: %d", got)
	}
	if got := e.routeExpectedGas(route[:2], execution.FundingV3FlashSwap, limit); got != 310_000 {
		t.Fatalf("the route without a wrap leg: %d", got)
	}
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, 320_000); got != 320_000 {
		t.Fatalf("not capped at the limit: %d", got)
	}
	// A wrap and a MetricOmm leg add their priors together.
	metric := []domain.PoolKey{{Kind: domain.PoolKindMetricOmm, Address: domain.Address{5}}, v4, wrapKey}
	if got := e.routeExpectedGas(metric, execution.FundingV3FlashSwap, limit); got != 310_000+metricLegExtraGas+planning.WrapLegGas {
		t.Fatalf("metric and wrap priors: %d", got)
	}
	// Its own measurements replace the prior.
	for _, used := range []uint64{352_000, 349_000, 351_000} {
		e.observeRouteGas(route, execution.FundingV3FlashSwap, used)
	}
	if got := e.routeExpectedGas(route, execution.FundingV3FlashSwap, limit); got != 352_000 {
		t.Fatalf("own history: %d", got)
	}
}

// The search finds WETH -> V3 -> USDT -> V4 (native ETH/USDT) -> ETH -> wrap
// -> WETH through the production ranking and quoting paths once the wrap
// pool is in the store, and nothing without it; planning then builds the
// executor route only with wrap_native on.
func TestSearchAndPlanningCrossANativeV4PoolThroughTheWrap(t *testing.T) {
	native, weth, usdt := domain.Address{}, domain.Address{0x82}, domain.Address{0xfd}
	q96 := new(big.Int).Lsh(big.NewInt(1), 96)
	liquidity, _ := new(big.Int).SetString("1000000000000000000000000", 10)
	v3 := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: addressAt(31)}, Token0: weth, Token1: usdt, Fee: 500, TickSpacing: 10, SqrtPriceX96: q96, Liquidity: liquidity}
	// ETH is a fifth cheaper in the V4 pool: sqrt price 0.9, price 0.81.
	cheap := new(big.Int).Quo(new(big.Int).Mul(q96, big.NewInt(9)), big.NewInt(10))
	v4 := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV4, Address: addressAt(32), PoolID: domain.Hash{1}}, Token0: native, Token1: usdt, Fee: 500, TickSpacing: 10, SqrtPriceX96: cheap, Tick: -2108, Liquidity: liquidity}
	bounds := func(token domain.Address) (arb.InputBounds, bool) {
		if token != weth {
			return arb.InputBounds{}, false
		}
		return arb.InputBounds{Min: big.NewInt(1e15), Max: big.NewInt(1e18)}, true
	}
	options := arb.RankedOptions{Bounds: bounds, SwapFee: dex.SwapFee, Model: dex.RankingModel}
	search := func(withWrap bool) (*poolstate.Store, []arb.Opportunity) {
		store := poolstate.New()
		for _, p := range []pool.State{v3, v4} {
			if err := store.Upsert(p); err != nil {
				t.Fatal(err)
			}
			if err := store.SetDeep(p.Key, pool.DeepState{}); err != nil {
				t.Fatal(err)
			}
		}
		store.ClearDirty()
		if withWrap {
			if _, err := setWrapPool(store, nil, weth); err != nil {
				t.Fatal(err)
			}
		}
		topology, err := pipeline.BuildTopology(t.Context(), store.Pools(), pipeline.TopologyOptions{Depth: 4, MaxCycles: 100, K: 4, Notional: func(domain.Address) *big.Int { return big.NewInt(1e16) }})
		if err != nil {
			t.Fatal(err)
		}
		result, err := topology.Engine.EvaluateRanked(t.Context(), store.View(), []domain.PoolKey{v4.Key}, options)
		if err != nil {
			t.Fatal(err)
		}
		return store, result.Opportunities
	}
	if _, found := search(false); len(found) != 0 {
		t.Fatalf("%d opportunities without the wrap pool", len(found))
	}
	store, found := search(true)
	if len(found) != 1 {
		t.Fatalf("%d opportunities through the wrap pool, want 1", len(found))
	}
	o := found[0]
	wrapKey := wrap.Key(weth)
	if o.Cycle.Start != weth || !slices.Equal(o.Cycle.Pools, []domain.PoolKey{v3.Key, v4.Key, wrapKey}) || len(o.Legs) != 3 {
		t.Fatalf("cycle %+v", o.Cycle)
	}
	last := o.Legs[2]
	if last.Pool != wrapKey || last.AmountIn.Cmp(o.Legs[1].AmountOut) != 0 || last.AmountOut.Cmp(last.AmountIn) != 0 || o.Output.Cmp(last.AmountOut) != 0 {
		t.Fatalf("wrap leg %s in, %s out; route output %s", last.AmountIn, last.AmountOut, o.Output)
	}
	// About 23% before fees and impact: the gap the V4 pool opens.
	if gain := new(big.Int).Quo(new(big.Int).Mul(o.Net, big.NewInt(10000)), o.Input).Int64(); gain < 1500 || gain > 2400 {
		t.Fatalf("net %s on %s in: %d bps", o.Net, o.Input, gain)
	}

	now := time.Now()
	cfg := planning.ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1}, GasLimit: 800000, MaxFee: big.NewInt(1000000), MaxTip: new(big.Int), TargetTip: new(big.Int), MaxTotalFee: big.NewInt(1e12), MinimumNet: big.NewInt(1), SlippageBPS: 50, Lifetime: 500 * time.Millisecond}
	costs := planning.ReactiveCosts{BaseFee: big.NewInt(294), Extra: new(big.Int), ExpiresMS: now.Add(time.Second).UnixMilli()}
	version := domain.StateVersion{Anchor: domain.BlockRef{Hash: domain.Hash{1}}, PrefixHash: domain.Hash{1}}
	conversion := execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}
	if _, err := planning.BuildReactive(t.Context(), store.View(), o, nil, nil, version, conversion, costs, cfg, now); err == nil || !strings.Contains(err.Error(), "wrap leg not enabled") {
		t.Fatalf("wrap_native off: %v", err)
	}
	cfg.WrapNative = true
	intent, err := planning.BuildReactive(t.Context(), store.View(), o, nil, nil, version, conversion, costs, cfg, now)
	if err != nil {
		t.Fatal(err)
	}
	steps := intent.Route.Steps
	if intent.Route.Funding != execution.FundingV3FlashSwap || len(steps) != 3 || steps[1].TokenOut != native || steps[2].Pool != wrapKey || steps[2].TokenIn != native || steps[2].TokenOut != weth {
		t.Fatalf("route %+v", intent.Route)
	}
	if err := intent.Validate(t.Context(), now); err != nil {
		t.Fatal(err)
	}
	// The turn that looks for a lending first pool leaves this route alone,
	// and never starts one on the wrap.
	if _, _, turned := flashSwapTurn(store.View(), o, func(domain.Address) bool { return true }); turned {
		t.Fatal("a route that already starts on a lending pool was turned")
	}
	reversed := o
	reversed.Cycle.Pools = []domain.PoolKey{wrapKey, v4.Key, v3.Key}
	reversed.Legs = []arb.Leg{{Pool: wrapKey, AmountIn: big.NewInt(1)}, {Pool: v4.Key, AmountIn: big.NewInt(1)}, {Pool: v3.Key, AmountIn: big.NewInt(7)}}
	cycle, principal, turned := flashSwapTurn(store.View(), reversed, func(token domain.Address) bool { return token == usdt })
	if !turned || cycle.Start != usdt || cycle.Pools[0] != v3.Key || cycle.Pools[1] != wrapKey || principal.Int64() != 7 {
		t.Fatalf("turned %v: %+v, principal %v", turned, cycle, principal)
	}
}
