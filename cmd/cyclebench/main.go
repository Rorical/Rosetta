// Command cyclebench benchmarks cycle search and V2 quote latency on a
// synthetic universe of disjoint rings, and reports whether the one known
// profitable ring is found and ranked first.
//
// The reserves are fabricated and the quote path uses exact V2 math on them, so
// the numbers are synthetic timing and recall only. They say nothing about
// on-chain accuracy or realizable profit.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"os"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

// config is the raw command-line configuration.
type config struct {
	depth     int
	cycles    int
	repeat    int
	warmup    int
	k         int
	notional  *big.Int
	maxCycles int
	poolTopK  int
	timing    bool
}

func main() {
	cfg, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, "cyclebench:", err)
		os.Exit(2)
	}
	if err := run(context.Background(), cfg, os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "cyclebench:", err)
		os.Exit(1)
	}
}

func parseFlags(args []string) (config, error) {
	cfg := config{
		depth:    10,
		cycles:   20,
		repeat:   20,
		warmup:   1,
		k:        200,
		notional: big.NewInt(1_000_000_000_000_000_000),
		timing:   true,
	}
	notionalText := cfg.notional.String()

	flags := flag.NewFlagSet("cyclebench", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.IntVar(&cfg.depth, "depth", cfg.depth, "legs per cycle (9..16)")
	flags.IntVar(&cfg.cycles, "cycles", cfg.cycles, "disjoint rings; the first is profitable")
	flags.IntVar(&cfg.repeat, "repeat", cfg.repeat, "measured evaluations")
	flags.IntVar(&cfg.warmup, "warmup", cfg.warmup, "evaluations discarded before measuring")
	flags.IntVar(&cfg.k, "k", cfg.k, "candidate cycles per dirty pool")
	flags.StringVar(&notionalText, "notional", notionalText, "input amount in base units")
	flags.IntVar(&cfg.maxCycles, "max-cycles", 0, "cycle table budget; 0 uses cycles*4")
	flags.IntVar(&cfg.poolTopK, "pool-top-k", 0, "cycles indexed per pool; 0 uses cycles*4")
	flags.BoolVar(&cfg.timing, "timing", cfg.timing,
		"measure and report search and quote latency; false runs one evaluation")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}

	if cfg.maxCycles == 0 {
		cfg.maxCycles = cfg.cycles * 4
	}
	if cfg.poolTopK == 0 {
		cfg.poolTopK = cfg.cycles * 4
	}

	notional, ok := new(big.Int).SetString(notionalText, 10)
	if !ok || notional.Sign() <= 0 {
		return config{}, fmt.Errorf("-notional %q: want a positive base-10 integer", notionalText)
	}
	cfg.notional = notional

	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (cfg config) validate() error {
	if cfg.depth < minDepth || cfg.depth > maxDepth {
		return fmt.Errorf("-depth %d: want %d..%d", cfg.depth, minDepth, maxDepth)
	}
	if cfg.cycles < 1 {
		return fmt.Errorf("-cycles %d: want at least 1", cfg.cycles)
	}
	if cfg.repeat < 1 {
		return fmt.Errorf("-repeat %d: want at least 1", cfg.repeat)
	}
	if cfg.warmup < 0 {
		return fmt.Errorf("-warmup %d: want non-negative", cfg.warmup)
	}
	if cfg.k < 1 {
		return fmt.Errorf("-k %d: want positive", cfg.k)
	}
	if cfg.maxCycles < 1 {
		return fmt.Errorf("-max-cycles %d: want positive", cfg.maxCycles)
	}
	if cfg.poolTopK < 1 {
		return fmt.Errorf("-pool-top-k %d: want positive", cfg.poolTopK)
	}
	if cfg.notional == nil || cfg.notional.Sign() <= 0 {
		return errors.New("-notional: want a positive integer")
	}
	return nil
}

// measurements returns the warmup and measured evaluation counts. Without
// timing, one evaluation is all the run needs.
func (cfg config) measurements() (warmup, repeat int) {
	if !cfg.timing {
		return 0, 1
	}
	return cfg.warmup, cfg.repeat
}

// run generates the synthetic universe, builds the deep cycle table and engine,
// measures the configured evaluations, and writes the report.
func run(ctx context.Context, cfg config, stdout io.Writer) error {
	universe, err := newUniverse(cfg.depth, cfg.cycles)
	if err != nil {
		return err
	}

	built, err := graph.Build(universe.pools)
	if err != nil {
		return fmt.Errorf("build graph: %w", err)
	}
	table, err := graph.BuildDeepTableRange(built, cfg.depth, cfg.depth, cfg.maxCycles, cfg.poolTopK)
	if err != nil && !errors.Is(err, graph.ErrTableFull) {
		return fmt.Errorf("build deep table: %w", err)
	}

	store := state.New()
	for _, shallow := range universe.pools {
		if err := store.Upsert(shallow); err != nil {
			return fmt.Errorf("upsert pool %s: %w", shallow.Key.Address.Hex(), err)
		}
	}
	dirty := store.Dirty()

	quoter, err := simulate.NewQuoter(func(shallow pool.State) (pool.Adapter, error) {
		return uniswapv2.New(shallow.Fee)
	})
	if err != nil {
		return fmt.Errorf("build quoter: %w", err)
	}
	timed := &timedQuoter{inner: quoter}
	engine, err := arb.New(table, func(domain.Address) *big.Int {
		return new(big.Int).Set(cfg.notional)
	}, cfg.k, timed)
	if err != nil {
		return fmt.Errorf("build engine: %w", err)
	}

	warmup, repeat := cfg.measurements()
	measured, err := measure(ctx, engine, store, dirty, warmup, repeat, timed)
	if err != nil {
		return err
	}

	writeReport(stdout, cfg, table.Len(), measured, universe.known)
	return nil
}

// measurement is the first measured evaluation plus the latency samples for
// search and quoting. Quote samples cover only measured evaluations: warmup
// quotes are discarded before sampling starts.
type measurement struct {
	opportunities []arb.Opportunity
	search        []time.Duration
	quote         []time.Duration
}

// measure warms up, resets the quote samples, then runs repeat evaluations. It
// keeps the first measured result; Evaluate is read-only and deterministic, so
// every call sees the same state and ranking.
func measure(ctx context.Context, engine *arb.Engine, view arb.View, dirty []domain.PoolKey, warmup, repeat int, timed *timedQuoter) (measurement, error) {
	for i := 0; i < warmup; i++ {
		if _, err := engine.Evaluate(ctx, view, dirty); err != nil {
			return measurement{}, fmt.Errorf("warmup evaluate: %w", err)
		}
	}
	timed.reset()

	measured := measurement{search: make([]time.Duration, 0, repeat)}
	for i := 0; i < repeat; i++ {
		start := time.Now()
		opportunities, err := engine.Evaluate(ctx, view, dirty)
		measured.search = append(measured.search, time.Since(start))
		if err != nil {
			return measurement{}, fmt.Errorf("evaluate: %w", err)
		}
		if i == 0 {
			measured.opportunities = opportunities
		}
	}
	measured.quote = timed.snapshot()
	return measured, nil
}

// writeReport prints the timing summary and whether the known ring was found.
// The first line states the synthetic scope because the numbers cannot be read
// as on-chain accuracy.
func writeReport(w io.Writer, cfg config, tableCycles int, measured measurement, known graph.Cycle) {
	rank := findRank(measured.opportunities, known)
	found := rank > 0
	net := "none"
	if found {
		net = measured.opportunities[rank-1].Net.String()
	}
	top1 := rank == 1

	fmt.Fprintln(w, "synthetic timing-only; not on-chain accuracy")
	fmt.Fprintf(w, "table depth=%d cycles=%d table_cycles=%d opportunities=%d\n",
		cfg.depth, cfg.cycles, tableCycles, len(measured.opportunities))
	if cfg.timing {
		writeTimingLine(w, "search", measured.search)
		writeTimingLine(w, "quote", measured.quote)
	}
	fmt.Fprintf(w, "known found=%t rank=%d net=%s legs=%d cycles=%d opportunities=%d\n",
		found, rank, net, cfg.depth, tableCycles, len(measured.opportunities))
	fmt.Fprintf(w, "top1=%t\n", top1)
	fmt.Fprintf(w, "synthetic timing-only depth=%d cycles=%d opportunities=%d found=%t rank=%d top1=%t\n",
		cfg.depth, cfg.cycles, len(measured.opportunities), found, rank, top1)
}

// findRank returns the 1-based rank of target among the sorted opportunities,
// or 0 when the engine did not produce it.
func findRank(opportunities []arb.Opportunity, target graph.Cycle) int {
	for i, opportunity := range opportunities {
		if sameDirectedCycle(opportunity.Cycle, target) {
			return i + 1
		}
	}
	return 0
}

// sameDirectedCycle compares two cycles in the canonical form the table stores:
// start at the ring's smallest token, pools in traversal order.
func sameDirectedCycle(a, b graph.Cycle) bool {
	if a.Start != b.Start || a.Len != b.Len || len(a.Pools) != len(b.Pools) {
		return false
	}
	for i := range a.Pools {
		if a.Pools[i] != b.Pools[i] {
			return false
		}
	}
	return true
}
