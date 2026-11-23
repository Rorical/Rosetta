// Command capturecheck compares historical arbitrage paths and quotes using
// pre-block state and optional replay of prior transaction logs.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "capturecheck:", err)
		os.Exit(2)
	}
	if err := run(context.Background(), cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "capturecheck:", err)
		os.Exit(1)
	}
}

// run seeds the case's pre-block state, optionally applies prior pool logs,
// and evaluates the cycle table. Quote differences are reported as results.
func run(parent context.Context, cfg config, stdout io.Writer) error {
	data, err := os.ReadFile(cfg.casePath)
	if err != nil {
		return fmt.Errorf("read case %s: %w", cfg.casePath, err)
	}
	c, err := decodeCase(data)
	if err != nil {
		return fmt.Errorf("%s: %w", cfg.casePath, err)
	}
	gas, err := resolveGas(cfg, c)
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(parent, cfg.timeout)
	defer cancel()

	client := rpc.NewClient(cfg.rpcURL, rpc.Options{
		Timeout:     30 * time.Second,
		MaxRetries:  3,
		Backoff:     500 * time.Millisecond,
		MinInterval: cfg.rpcInterval,
		MaxCalls:    cfg.rpcMaxCalls,
	})
	cache := newCachedCaller(rpcCaller{client: client}, cfg.cacheDir)

	store := state.New()

	block, err := cachedBlockByNumber(ctx, client, cache, c.PreBlock)
	if err != nil {
		return fmt.Errorf("header %d: %w", c.PreBlock, err)
	}
	ref := domain.BlockRef{
		Number:     block.Number,
		Hash:       block.Hash,
		ParentHash: block.ParentHash,
		Timestamp:  block.Timestamp,
		L1Block:    block.L1BlockNumber,
	}

	if err := bootstrap.Seed(ctx, cache, store, seedSpecs(c), ref, int16(cfg.wordRadius)); err != nil {
		return fmt.Errorf("seed at block %d: %w", ref.Number, err)
	}

	if cfg.replay {
		stats, err := replayPriorEvents(ctx, client, cache, store, c)
		if err != nil {
			return err
		}
		fmt.Fprintf(stdout, "replay block=%d prior_receipts=%d applied_logs=%d\n",
			stats.Block, stats.PriorReceipts, stats.AppliedLogs)
	}

	if err := evaluateCase(ctx, cfg, c, gas, store, stdout); err != nil {
		return err
	}
	fmt.Fprintln(stdout, cache.summary())
	if reason := cache.disabledReason(); reason != "" {
		fmt.Fprintf(stdout, "cache disabled: %s\n", reason)
	}
	return nil
}

// evaluateCase builds the graph, cycle table, and quoting engine over store,
// runs the configured measurements, and writes the report and timing lines.
func evaluateCase(ctx context.Context, cfg config, c caseData, gas arb.GasConfig, store *state.Store, stdout io.Writer) error {
	built, err := graph.Build(store.Pools())
	if err != nil {
		return fmt.Errorf("build graph: %w", err)
	}
	table, err := graph.BuildTable(built, cfg.depth, cfg.maxCycles)
	if err != nil && !errors.Is(err, graph.ErrTableFull) {
		return fmt.Errorf("build cycle table: %w", err)
	}

	quoter, err := simulate.NewQuoter(captureAdapter)
	if err != nil {
		return fmt.Errorf("build quoter: %w", err)
	}
	timed := &timedQuoter{inner: quoter}

	pools := store.Pools()
	notional := notionalByToken(c.OnChain.Legs)
	engine, err := arb.New(table, func(start domain.Address) *big.Int {
		return notional[start]
	}, cfg.k, timed)
	if err != nil {
		return fmt.Errorf("build engine: %w", err)
	}
	engine = engine.WithGas(gas)

	dirty := make([]domain.PoolKey, len(pools))
	for i, shallow := range pools {
		dirty[i] = shallow.Key
	}

	measured, err := measure(ctx, cfg, engine, store, dirty, c.OnChain, timed)
	if err != nil {
		return err
	}
	opportunities, direct := measured.Opportunities, measured.Direct

	rank := 0
	tokens := tokensFromStore(store)
	onchain := c.onChainCycle()
	for i, opportunity := range opportunities {
		if sameCycle(opportunity.Cycle, onchain, tokens) {
			rank = i + 1
			break
		}
	}

	writeReport(stdout, c, table.Len(), opportunities, direct, rank, cfg.top)
	if cfg.timing {
		measured.Timing.writeTo(stdout)
	}
	return nil
}

// resolveGas applies the case's gas figures and lets explicit flags replace
// them.
func resolveGas(cfg config, c caseData) (arb.GasConfig, error) {
	gas := arb.GasConfig{
		Units: c.OnChain.GasUsed,
		Price: new(big.Int).Set(c.OnChain.GasPrice),
		Token: c.OnChain.Start,
	}
	if cfg.gasToken != "" {
		token, err := domain.ParseAddress(cfg.gasToken)
		if err != nil {
			return arb.GasConfig{}, fmt.Errorf("-gas-token %q: %w", cfg.gasToken, err)
		}
		gas.Token = token
	}
	if cfg.gasUnits != "" {
		units, err := strconv.ParseUint(cfg.gasUnits, 10, 64)
		if err != nil {
			return arb.GasConfig{}, fmt.Errorf("-gas-units %q: %w", cfg.gasUnits, err)
		}
		gas.Units = units
	}
	if cfg.gasPrice != "" {
		price, ok := new(big.Int).SetString(cfg.gasPrice, 10)
		if !ok || price.Sign() < 0 {
			return arb.GasConfig{}, fmt.Errorf("-gas-price %q: want a non-negative base-10 integer", cfg.gasPrice)
		}
		gas.Price = price
	}
	return gas, nil
}

func seedSpecs(c caseData) []bootstrap.Spec {
	specs := make([]bootstrap.Spec, len(c.Universe))
	for i, p := range c.Universe {
		specs[i] = bootstrap.Spec{
			Address:     p.Address,
			Token0:      p.Token0,
			Token1:      p.Token1,
			Kind:        p.Kind,
			Fee:         p.Fee,
			TickSpacing: p.TickSpacing,
		}
	}
	return specs
}

// captureAdapter selects the mathematical adapter for the two kinds this
// harness models. Other kinds fail loudly instead of being priced with the
// wrong curve.
func captureAdapter(shallow pool.State) (pool.Adapter, error) {
	switch shallow.Key.Kind {
	case domain.PoolKindV3:
		return uniswapv3.New(shallow.Fee, shallow.TickSpacing)
	case domain.PoolKindSlipstream:
		return slipstream.New(shallow.Fee, shallow.TickSpacing)
	default:
		return nil, fmt.Errorf("%w: pool %s kind %d",
			domain.ErrUnsupported, shallow.Key.Address, shallow.Key.Kind)
	}
}

// directQuote is the executed path repriced leg by leg at the case's input.
type directQuote struct {
	Output *big.Int
	Net    *big.Int
	Legs   []directLeg
}

type directLeg struct {
	Pool      domain.Address
	AmountIn  *big.Int
	AmountOut *big.Int
}

// quoteOnChain walks the executed path in its own order and token direction.
func quoteOnChain(ctx context.Context, quoter arb.Quoter, store *state.Store, onchain onChainData) (directQuote, error) {
	amount := new(big.Int).Set(onchain.Input)
	quote := directQuote{Legs: make([]directLeg, 0, len(onchain.Path))}
	for i, spec := range onchain.Path {
		key := spec.key()
		shallow, ok := store.Pool(key)
		if !ok {
			return directQuote{}, fmt.Errorf("direct quote: pool %s missing from store", spec.Address)
		}
		deep, ok := store.Deep(key)
		if !ok {
			return directQuote{}, fmt.Errorf("direct quote: deep state for pool %s missing from store", spec.Address)
		}
		out, err := quoter.QuoteExactIn(ctx, shallow, deep, onchain.Legs[i].TokenIn, amount)
		if err != nil {
			return directQuote{}, fmt.Errorf("direct quote leg %d pool %s: %w", i+1, spec.Address, err)
		}
		quote.Legs = append(quote.Legs, directLeg{Pool: spec.Address, AmountIn: amount, AmountOut: out})
		amount = out
	}
	quote.Output = amount
	quote.Net = new(big.Int).Sub(amount, onchain.Input)
	return quote, nil
}

// tokensFromStore resolves a pool's token pair from the seeded state.
func tokensFromStore(store *state.Store) poolTokens {
	return func(key domain.PoolKey) (domain.Address, domain.Address, bool) {
		shallow, ok := store.Pool(key)
		if !ok {
			return domain.Address{}, domain.Address{}, false
		}
		return shallow.Token0, shallow.Token1, true
	}
}

var _ arb.View = (*state.Store)(nil)
