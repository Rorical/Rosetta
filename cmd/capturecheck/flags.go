package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"time"
)

// config is the raw command-line configuration.
type config struct {
	casePath    string
	rpcURL      string
	rpcInterval time.Duration
	rpcMaxCalls uint64
	cacheDir    string
	replay      bool
	wordRadius  int
	depth       int
	k           int
	maxCycles   int
	top         int
	timeout     time.Duration
	repeat      int
	warmup      int
	timing      bool
	gasToken    string
	gasUnits    string
	gasPrice    string
}

func parseFlags(args []string) (config, error) {
	var cfg config
	flags := flag.NewFlagSet("capturecheck", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&cfg.casePath, "case", "", "case JSON file (required)")
	flags.StringVar(&cfg.rpcURL, "rpc", "https://arb1.arbitrum.io/rpc", "RPC endpoint")
	flags.DurationVar(&cfg.rpcInterval, "rpc-min-interval", 0, "minimum interval between external HTTP attempts")
	flags.Uint64Var(&cfg.rpcMaxCalls, "rpc-max-calls", 0, "maximum RPC method calls including retries/batch members; zero is unlimited")
	flags.StringVar(&cfg.cacheDir, "cache", defaultCacheDir(), "eth_call disk cache directory; empty disables it")
	flags.BoolVar(&cfg.replay, "replay", true, "replay the target block's prior universe-pool logs over the block-1 state")
	flags.IntVar(&cfg.wordRadius, "word-radius", 2, "V3/Slipstream bitmap words loaded on each side of the current tick")
	flags.IntVar(&cfg.depth, "depth", 4, "maximum cycle depth")
	flags.IntVar(&cfg.k, "k", 200, "candidate cycles per dirty pool")
	flags.IntVar(&cfg.maxCycles, "max-cycles", 500_000, "cycle table budget")
	flags.IntVar(&cfg.top, "top", 10, "opportunities to print")
	flags.DurationVar(&cfg.timeout, "timeout", 5*time.Minute, "overall deadline")
	flags.IntVar(&cfg.repeat, "repeat", 10, "measured evaluations")
	flags.IntVar(&cfg.warmup, "warmup", 1, "evaluations discarded before measuring")
	flags.BoolVar(&cfg.timing, "timing", true, "measure and report search and quote latency; false runs one evaluation")
	flags.StringVar(&cfg.gasToken, "gas-token", "", "gas token; empty uses the case's on-chain start token")
	flags.StringVar(&cfg.gasUnits, "gas-units", "", "gas units; empty uses the case's gas_used")
	flags.StringVar(&cfg.gasPrice, "gas-price", "", "gas price in the gas token's base units; empty uses the case's gas_price")
	if err := flags.Parse(args); err != nil {
		return config{}, err
	}
	if err := cfg.validate(); err != nil {
		return config{}, err
	}
	return cfg, nil
}

func (cfg *config) validate() error {
	if cfg.casePath == "" {
		return errors.New("-case is required")
	}
	if cfg.rpcURL == "" {
		return errors.New("-rpc must not be empty")
	}
	if cfg.rpcInterval < 0 {
		return errors.New("-rpc-min-interval must not be negative")
	}
	if cfg.wordRadius < 0 || cfg.wordRadius > math.MaxInt16 {
		return fmt.Errorf("-word-radius %d: want 0..%d", cfg.wordRadius, math.MaxInt16)
	}
	if cfg.depth < 2 {
		return fmt.Errorf("-depth %d: want at least 2", cfg.depth)
	}
	if cfg.k <= 0 {
		return fmt.Errorf("-k %d: want positive", cfg.k)
	}
	if cfg.maxCycles <= 0 {
		return fmt.Errorf("-max-cycles %d: want positive", cfg.maxCycles)
	}
	if cfg.top < 0 {
		return fmt.Errorf("-top %d: want non-negative", cfg.top)
	}
	if cfg.timeout <= 0 {
		return fmt.Errorf("-timeout %s: want positive", cfg.timeout)
	}
	if cfg.repeat < 1 {
		return fmt.Errorf("-repeat %d: want at least 1", cfg.repeat)
	}
	if cfg.warmup < 0 {
		return fmt.Errorf("-warmup %d: want non-negative", cfg.warmup)
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
