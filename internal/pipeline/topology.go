package pipeline

import (
	"context"
	"errors"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
)

// Topology is immutable after construction. Only the state writer can
// install it; graph enumeration and adapter assembly can run in the background.
type Topology struct {
	markets      []backtest.PoolMetadata
	Table        *graph.Table
	Engine       *arb.Engine
	Fast         *arb.Engine // two- and three-hop routes of Engine, indexed by pool, for the frame path
	DynamicPools []domain.PoolKey
	NeedsLocal   bool
	Truncated    bool
}

func BuildTopology(ctx context.Context, pools []pool.State, config TopologyOptions) (*Topology, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	pools = filterRiskPools(pools, config.TokenCheck, config.PoolCheck)
	built, err := graph.Build(pools)
	if err != nil {
		return nil, err
	}
	table, err := graph.BuildPackedBalancedContext(ctx, built, pools, config.Depth, config.MaxCycles)
	if err != nil && !errors.Is(err, graph.ErrTableFull) {
		return nil, err
	}
	truncated := errors.Is(err, graph.ErrTableFull)
	return assembleTopology(ctx, pools, config, table, truncated, built)
}

// ExtendTopology is used by background discovery for append-only catalogs.
// Untruncated extensions retain path identities. A full table is rebuilt with
// balanced coverage; the replacing graph version retires the old path IDs.
func ExtendTopology(ctx context.Context, pools []pool.State, previous *Topology, added []domain.PoolKey, config TopologyOptions) (*Topology, error) {
	filtered := filterRiskPools(pools, config.TokenCheck, config.PoolCheck)
	if len(filtered) != len(pools) {
		return BuildTopology(ctx, filtered, config)
	}
	built, err := graph.Build(pools)
	if err != nil {
		return nil, err
	}
	var table *graph.Table
	if previous.Truncated || previous.Table.Len() >= config.MaxCycles {
		table, err = graph.BuildPackedBalancedContext(ctx, built, pools, config.Depth, config.MaxCycles)
	} else {
		table, err = graph.ExtendPackedContext(ctx, built, pools, previous.Table, added, config.Depth, config.MaxCycles)
		if errors.Is(err, graph.ErrTableFull) {
			table = nil // Release the partial extension before rebuilding.
			table, err = graph.BuildPackedBalancedContext(ctx, built, pools, config.Depth, config.MaxCycles)
		}
	}
	if err != nil && !errors.Is(err, graph.ErrTableFull) {
		return nil, err
	}
	topology, buildErr := assembleTopology(ctx, pools, config, table, errors.Is(err, graph.ErrTableFull), built)
	if buildErr == nil {
		topology.Engine.RememberCycles(previous.Engine.ObservedCycles())
		topology.Engine.InheritPhantoms(previous.Engine)
		if topology.Fast != nil {
			topology.Fast.InheritPhantoms(previous.Fast)
		}
	}
	return topology, buildErr
}

func assembleTopology(ctx context.Context, pools []pool.State, config TopologyOptions, table *graph.Table, truncated bool, built *graph.Graph) (*Topology, error) {
	packed, err := graph.PackTable(ctx, table, pools)
	if err != nil {
		return nil, err
	}
	table = packed
	quoter, err := simulate.NewQuoter(func(p pool.State) (pool.Adapter, error) {
		if err := CheckPoolRisk(p, config.TokenCheck, config.PoolCheck); err != nil {
			return nil, err
		}
		return dex.NewAdapter(p)
	})
	if err != nil {
		return nil, err
	}
	engine, err := arb.New(table, config.Notional, config.K, quoter)
	if err != nil {
		return nil, err
	}
	if err := engine.PreparePaths(ctx, pools); err != nil {
		return nil, err
	}
	limits := exploreLimits(config.Depth, config.ExplorePaths)
	if err := engine.EnableBackgroundExploration(built, limits); err != nil {
		return nil, err
	}
	hops := config.FastLaneHops
	if hops == 0 {
		hops = fastLaneHops
	}
	quotes := config.FastLaneQuotes
	if quotes == 0 {
		quotes = fastLaneQuotes
	}
	fast, err := engine.ShortEngine(ctx, hops, quotes, config.FastCoarseCycles)
	if err != nil {
		return nil, err
	}
	topology := &Topology{Table: table, Engine: engine, Fast: fast, Truncated: truncated}
	for _, p := range pools {
		topology.markets = append(topology.markets, backtest.PoolMetadata{Key: p.Key, Token0: p.Token0, Token1: p.Token1})
		if p.Key.Kind != domain.PoolKindV2 {
			topology.NeedsLocal = true
		}
		if DynamicFee(p.Key.Kind) {
			topology.DynamicPools = append(topology.DynamicPools, p.Key)
		}
	}
	return topology, nil
}

// By default the fast lane quotes routes of at most three hops, two per
// funding asset. Short routes carry most OP arbitrage (observed routes run
// 2–4 hops).
const fastLaneHops, fastLaneQuotes = 3, 2

// defaultExplorePaths is the retained-cache size used before search configs
// could raise it; zero from programmatic callers keeps the same behavior.
const defaultExplorePaths = 8192

// exploreLimits applies the configured cache cap over the depth defaults.
func exploreLimits(depth, cachePaths int) graph.ExploreLimits {
	limits := graph.DefaultExploreLimits(depth)
	if cachePaths == 0 {
		cachePaths = defaultExplorePaths
	}
	limits.CachePaths = cachePaths
	return limits
}

type TopologyOptions struct {
	TokenCheck          func(domain.Address) error
	PoolCheck           func(domain.PoolKey) error // quarantined pools
	Depth, MaxCycles, K int
	// ExplorePaths caps the background explorer's retained path cache; zero
	// is 8192.
	ExplorePaths int
	// FastCoarseCycles bounds the fast lane's ranked routes per dirty pool;
	// zero ranks them all. The full search worker is never bounded.
	FastCoarseCycles int
	// FastLaneHops is the longest route the fast lane holds; zero is three.
	FastLaneHops int
	// FastLaneQuotes is the fast lane's exact quotes per funding asset; zero is two.
	FastLaneQuotes int
	Notional       arb.NotionalFunc
}

// DynamicFee identifies adapters whose live fee can change without a pool event.
// Solidly V3 is absent: its fee changes only through a logged SetFee.
func DynamicFee(kind domain.PoolKind) bool {
	return kind == domain.PoolKindCamelotV3 || kind == domain.PoolKindSlipstream || kind == domain.PoolKindV4 || kind == domain.PoolKindVelodromeV2
}
