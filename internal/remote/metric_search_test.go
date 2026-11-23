package remote

import (
	"encoding/binary"
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

// metricMarket is the real WETH/USDC MetricOmm pool (words and quote of the
// fixtures) next to a Uniswap V3 pool of the same pair whose price is the
// pool's bid moved by shiftBPS: below it, WETH bought on V3 sells to the
// MetricOmm pool at a profit.
func metricMarket(t *testing.T, f metricFixture, shiftBPS int64, now time.Time) (*poolstate.Store, pool.State, pool.State) {
	t.Helper()
	metric := f.state()
	// The oracle's word with its timestamp moved to now: the quote is real,
	// its age is the test's.
	word := f.update
	binary.BigEndian.PutUint64(word[23:31], uint64(now.UnixMilli()))
	if err := metricomm.Refresh(metric.Metric, f.before, [3]domain.Hash{f.config, f.parameters, f.skewState}, word, uint64(now.Unix())); err != nil {
		t.Fatal(err)
	}
	if metric.Metric.BidX64 == nil {
		t.Fatal("fixture gives no quote")
	}
	// USDC per WETH in raw units is bid / 2^64 / 1e12; sqrtPriceX96 is its
	// root times 2^96.
	price := new(big.Int).Mul(metric.Metric.BidX64, big.NewInt(10_000+shiftBPS))
	price.Lsh(price, 192-64).Quo(price, big.NewInt(10_000)).Quo(price, metric.Metric.Token1Scale)
	sqrtPrice := new(big.Int).Sqrt(price)
	tick, err := math.SqrtPriceX96ToTick(sqrtPrice)
	if err != nil {
		t.Fatal(err)
	}
	v3 := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV3, Address: addressAt(30)}, Token0: metric.Token0, Token1: metric.Token1,
		Fee: 500, TickSpacing: 10, SqrtPriceX96: sqrtPrice, Tick: tick, Liquidity: new(big.Int).Lsh(big.NewInt(1), 70)}
	store := poolstate.New()
	for _, p := range []pool.State{metric, v3} {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	// No initialized tick near the price: the quote stays in range.
	if err := store.SetDeep(v3.Key, pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}, WindowMin: tick - 50_000, WindowMax: tick + 50_000}); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginBlock(domain.BlockRef{Number: 10, Hash: domain.Hash{10}, ParentHash: domain.Hash{9}, Timestamp: uint64(now.Unix())}); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	store.ClearDirty()
	return store, metric, v3
}

func metricTopology(t *testing.T, store *poolstate.Store) *pipeline.Topology {
	t.Helper()
	topology, err := pipeline.BuildTopology(t.Context(), store.Pools(), pipeline.TopologyOptions{Depth: 2, MaxCycles: 100, K: 4, Notional: func(domain.Address) *big.Int { return big.NewInt(100_000_000) }})
	if err != nil {
		t.Fatal(err)
	}
	if topology.Fast == nil {
		t.Fatal("topology has no fast lane")
	}
	return topology
}

// metricSearchOptions funds USDC from 1 to 5,000 units, with the production
// fee and ranking models.
func metricSearchOptions(usdc domain.Address) arb.RankedOptions {
	return arb.RankedOptions{SwapFee: dex.SwapFee, Model: dex.RankingModel, Bounds: func(token domain.Address) (arb.InputBounds, bool) {
		return arb.InputBounds{Min: big.NewInt(1_000_000), Max: big.NewInt(5_000_000_000)}, token == usdc
	}}
}

// The search prices a MetricOmm pool like any other edge: when a V3 pool
// sells WETH one percent below the pool's bid it finds the two-hop cycle and
// sizes it with exact quotes of both pools. A stale pool, or one whose price
// agrees with the V3 pool, gives no opportunity and no error.
func TestSearchFindsMetricOmmCycleAgainstV3(t *testing.T) {
	f := loadMetricFixture(t)
	now := time.Now()
	store, metric, v3 := metricMarket(t, f, -100, now)
	topology := metricTopology(t, store)
	options := metricSearchOptions(metric.Token1)

	for _, engine := range []*arb.Engine{topology.Engine, topology.Fast} {
		result, err := engine.EvaluateRanked(t.Context(), store.View(), []domain.PoolKey{metric.Key}, options)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.Opportunities) == 0 {
			t.Fatalf("no cycle through the mispriced pools: %+v", result.Issues)
		}
		best := result.Opportunities[0]
		if best.Cycle.Start != metric.Token1 || len(best.Legs) != 2 || best.Legs[0].Pool != v3.Key || best.Legs[1].Pool != metric.Key || best.Net.Sign() <= 0 {
			t.Fatalf("unexpected best route: %+v net %v", best.Cycle, best.Net)
		}
		// The MetricOmm leg is the adapter's own quote, not the ranking model's.
		exact, err := metricomm.New().QuoteExactIn(t.Context(), metric, pool.DeepState{}, metric.Token0, best.Legs[1].AmountIn)
		if err != nil || exact.Cmp(best.Legs[1].AmountOut) != 0 {
			t.Fatalf("leg output %s, adapter %v (%v)", best.Legs[1].AmountOut, exact, err)
		}

		stale := store.Fork()
		broken := metric.Copy()
		broken.Metric.Stale = true
		if err := stale.Upsert(broken); err != nil {
			t.Fatal(err)
		}
		result, err = engine.EvaluateRanked(t.Context(), stale.View(), []domain.PoolKey{metric.Key}, options)
		if err != nil || len(result.Opportunities) != 0 {
			t.Fatalf("a stale pool was quoted: %v, %d opportunities", err, len(result.Opportunities))
		}
	}

	// At the pool's own price the round trip only pays the spread and the fee.
	fair, same, _ := metricMarket(t, f, 0, now)
	result, err := metricTopology(t, fair).Engine.EvaluateRanked(t.Context(), fair.View(), []domain.PoolKey{same.Key}, options)
	if err != nil || len(result.Opportunities) != 0 {
		t.Fatalf("a fairly priced pair gave an opportunity: %v, %d", err, len(result.Opportunities))
	}
}

// An oracle update seen on the Fast feed reprices the pool and marks it
// changed, and the reactive fast lane searches it like any other changed
// pool: it builds an intent whose plan carries the MetricOmm leg as kind 8
// without a price limit or bound.
func TestOracleUpdateReachesTheFastLaneAndBuildsAMetricOmmIntent(t *testing.T) {
	f := loadMetricFixture(t)
	now := time.Now()
	store, metric, v3 := metricMarket(t, f, -100, now)
	// Before the update the pool has no quote: nothing to search.
	unquoted := metric.Copy()
	metricomm.ForgetQuote(unquoted.Metric)
	if err := store.Upsert(unquoted); err != nil {
		t.Fatal(err)
	}
	store.ClearDirty()
	topology := metricTopology(t, store)
	usdc := metric.Token1

	cfg := planning.ReactivePolicy{DeadlineSeconds: 2, Chain: domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161}, Sender: domain.Address{8}, Executor: domain.Address{9}, CodeHash: domain.Hash{1},
		GasLimit: 400_000, MaxFee: big.NewInt(100), MaxTip: new(big.Int), TargetTip: new(big.Int), MaxTotalFee: big.NewInt(100_000_000), MinimumNet: big.NewInt(1), SlippageBPS: 50, Lifetime: 500 * time.Millisecond,
		PricePrecheck: true, CumulativePrecheck: true, MetricOmm: true}
	s := &reactiveRuntime{jobs: make(chan struct{}, 1), settings: reactiveSettings{planning: cfg, wrapped: usdc, minimum: map[domain.Address]*big.Int{usdc: big.NewInt(1)},
		limits:   execution.DispatchLimits{Assets: map[domain.Address]*big.Int{usdc: big.NewInt(5_000_000_000)}},
		document: reactivePolicy{MaxCalldataBytes: 4096, FastLane: true}}}
	s.cache.Store(&reactiveCache{costs: planning.ReactiveCosts{BaseFee: big.NewInt(1), Extra: new(big.Int), ExpiresMS: now.Add(time.Minute).UnixMilli()}, wallet: map[domain.Address]*big.Int{usdc: big.NewInt(5_000_000_000)}, native: big.NewInt(1_000_000_000), block: store.Block()})
	r := &runtime{cfg: config.Config{Network: config.Network{Family: domain.FamilyNitro, ChainID: 42161, BlockTimeMS: 250}}, reactive: s, store: store, topology: topology, searchOptions: metricSearchOptions(usdc)}

	branch, err := ingest.NewFastBranch(store, ingest.FastOptions{})
	if err != nil {
		t.Fatal(err)
	}
	word := f.update
	binary.BigEndian.PutUint64(word[23:31], uint64(now.UnixMilli()))
	update := oracleTransaction(t, f, word, true, 1)
	dirty, err := branch.Apply(t.Context(), feed.FastMessage{BlockNumber: 11, TxHash: update.Hash, RawTx: update.Raw,
		Receipt: feed.FastReceipt{Successful: true, Logs: []feed.FastLog{{Address: f.oracle, Topics: update.Logs[0].Topics}}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirty) != 1 || dirty[0] != metric.Key {
		t.Fatalf("the oracle update must mark its pool changed: %v", dirty)
	}
	view, version, _, ok := branch.View()
	if !ok {
		t.Fatal("no tentative view")
	}
	r.tentative = version

	r.fastLane(t.Context(), view, dirty, version, now)
	s.offerMu.Lock()
	offer := s.fastSlot
	s.fastSlot = nil
	s.offerMu.Unlock()
	if offer == nil {
		t.Fatal("the fast lane queued nothing for the repriced pool", s.takeSkips())
	}
	// The build worker is not running in this fixture; build the offer here.
	r.buildReactive(t.Context(), offer)
	if len(s.jobs) != 1 {
		t.Fatal("no intent built", s.takeSkips())
	}
	<-s.jobs
	route := s.takeJob(s.primary()).intent.Route
	if len(route.Steps) != 2 || route.Steps[0].Pool != v3.Key || route.Steps[1].Pool != metric.Key {
		t.Fatalf("unexpected route: %+v", route.Steps)
	}
	leg := route.Steps[1]
	if leg.Pool.Kind != domain.PoolKindMetricOmm || leg.SqrtPriceLimitX96 != nil || leg.PriceBound != nil || leg.MinAmountOut.Sign() <= 0 || leg.Fee != 0 {
		t.Fatalf("MetricOmm leg carries a limit, bound or fee: %+v", leg)
	}
	// The V3 leg keeps its precheck; the MetricOmm pool never lends.
	if route.Steps[0].PriceBound == nil || route.MinRate == nil || route.Lender.Kind == domain.PoolKindMetricOmm || route.Hot {
		t.Fatalf("precheck or funding differ: %+v", route)
	}
	if _, err := execution.EncodePackedExecutorPlan(t.Context(), route); err != nil {
		t.Fatal(err)
	}
}
