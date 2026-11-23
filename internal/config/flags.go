package config

import (
	"flag"
	"fmt"
	"io"
	"time"
)

// Parse reads explicit command-line options. Fast Feed authentication is read
// separately from ROSETTA_FAST_FEED_KEY when that transport is enabled.
func Parse(args []string, output io.Writer) (Config, error) {
	flags := flag.NewFlagSet("rosetta", flag.ContinueOnError)
	flags.SetOutput(output)

	var cfg Config
	flags.StringVar(&cfg.TokenRiskFile, "token-risk-file", "", "chain-bound persistent transfer-shortfall exclusions shared by search and execution")
	flags.StringVar(&cfg.PoolRiskFile, "pool-risk-file", "", "chain-bound persistent pool quarantine from repeated execution reverts")
	flags.StringVar(&cfg.EventSource, "event-source", "auto", "event input: auto, rpc, sequencer or flashblocks")
	flags.StringVar(&cfg.FlashblocksURL, "flashblocks", "", "raw Flashblocks/Subblocks WebSocket stream; requires an OP Stack network; defaults to the network profile's stream unless -event-source rpc")
	flags.StringVar(&cfg.SubmitURL, "submit-rpc", "", "sequencer submission endpoint for signed transactions; defaults to the network profile's submit URL")
	flags.StringVar(&cfg.PoolConfig, "pool-config", "", "RPC bootstrap pool specifications JSON; alternative to -pool-snapshot")
	flags.StringVar(&cfg.PoolCatalog, "pool-catalog", "", "hash-bound token/factory catalog JSON; alternative to pool-config and pool-snapshot")
	flags.Uint64Var(&cfg.RPCMaxCalls, "rpc-max-calls", 0, "external request-unit budget per run including retries and batch members; 0 is unlimited")
	flags.BoolVar(&cfg.FlashblocksReceiptsRPC, "flashblocks-receipts-rpc", false, "allow RPC receipt hydration for stripped Flashblocks streams; may use many method calls")
	flags.DurationVar(&cfg.RPCMinInterval, "rpc-min-interval", 0, "minimum pacing interval per external method (batch members count separately); 0 disables pacing")
	flags.StringVar(&cfg.RPCSearchRefresh, "rpc-search-refresh", "canonical", "candidate state refresh: canonical RPC dependencies or logs for paper observation")
	flags.DurationVar(&cfg.RPCPoll, "rpc-poll", 2*time.Second, "canonical RPC head polling interval")
	flags.StringVar(&cfg.RPCHeadsURL, "rpc-heads", "", "optional WebSocket RPC whose newHeads subscription wakes the follower with each new block")
	flags.StringVar(&cfg.NetworkName, "network", "", "L2 preset: arbitrum-one (default), robinhood, robinhood-testnet, base, optimism, unichain or ink")
	flags.StringVar(&cfg.NetworkFile, "network-file", "", "custom ETH-native Arbitrum Nitro or OP Stack L2 network JSON profile")
	flags.StringVar(&cfg.RPCURL, "rpc", "", "L2 JSON-RPC endpoint for canonical blocks, state reads and reorg ancestor lookup")
	flags.StringVar(&cfg.FeedURL, "feed", "", "sequencer feed URL")
	flags.IntVar(&cfg.Blocks, "blocks", 0, "blocks to process; 0 runs until interrupted")
	flags.BoolVar(&cfg.JSONOutput, "json", false, "emit one JSON object per processed block")
	flags.StringVar(&cfg.PoolSnapshot, "pool-snapshot", "", "pool snapshot at the selected canonical anchor")
	flags.StringVar(&cfg.PoolSnapshotOut, "pool-snapshot-out", "", "write accepted pool state at shutdown; requires initialized pool state and a new file")
	flags.StringVar(&cfg.FastURL, "fast-feed", "", "optional Fast Feed WebSocket URL (reads ROSETTA_FAST_FEED_KEY)")
	flags.Int64Var(&cfg.FastFirstIndex, "fast-first-index", -1, "first Fast transaction index per block; must be measured before use")
	flags.Int64Var(&cfg.FastCanonicalFirstIndex, "fast-canonical-first-index", -1, "standard receipt index corresponding to the first Fast transaction")
	flags.StringVar(&cfg.SearchConfig, "search-config", "", "optional JSON funding bounds and dirty-path search configuration")
	flags.StringVar(&cfg.ReplayOut, "replay-out", "", "write canonical pool snapshots and full receipt logs to a new local JSONL capture")
	flags.StringVar(&cfg.ResearchConfig, "research-config", "", "optional observation limits and explicit per-asset cost budgets")
	flags.StringVar(&cfg.ResearchOut, "research-out", "", "write bounded candidate economics, survival samples and confirmation outcomes to a new JSONL file")
	flags.StringVar(&cfg.CandidateOut, "candidate-out", "", "write unconfirmed candidate JSON lines to a new file; requires -search-config")
	flags.StringVar(&cfg.DiscoveryConfig, "discovery-config", "", "optional background factory discovery JSON; requires search and pool state")
	flags.BoolVar(&cfg.ReactivePreflight, "reactive-preflight", false, "validate deployed executor and cached trading context, then exit without signing")
	flags.StringVar(&cfg.ReactiveConfig, "reactive-config", "", "explicit unconfirmed reactive execution policy; shadow or live")
	flags.StringVar(&cfg.ExecutionPaperConfig, "execution-paper-config", "", "read-only OP executor simulation policy and persistent sample journal")
	flags.StringVar(&cfg.ExecutionConfig, "execution-config", "", "optional executor, funding, fee and valuation policy; enables full executor confirmation")
	flags.StringVar(&cfg.PlanOut, "plan-out", "", "write versioned ready plan JSON lines to a new file; requires -execution-config")
	flags.StringVar(&cfg.MetricsListen, "metrics-listen", "", "optional metrics TCP address, for example 127.0.0.1:9090; serves /metrics")
	flags.StringVar(&cfg.ProfileListen, "profile-listen", "", "optional loopback TCP address serving Go runtime profiles under /debug/pprof/")
	flags.StringVar(&cfg.ControlDir, "control-dir", "", "private directory for the operator socket and persistent delivery gate; holds one instance")
	flags.StringVar(&cfg.MetricsOut, "metrics-out", "", "write final pipeline metrics JSON to a new file after workers stop")
	flags.StringVar(&cfg.RunRoot, "run-root", "", "create run directories under DIR/runs and rotate candidate, research, replay, snapshot and metrics outputs in-process; requires -control-dir and -search-config")
	flags.DurationVar(&cfg.RPCWait, "rpc-wait", 30*time.Second, "maximum wait for a null RPC header during startup and recovery; 0 fails immediately")

	if err := flags.Parse(args); err != nil {
		return Config{}, err
	}
	if flags.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected positional argument %q", flags.Arg(0))
	}
	if err := cfg.ValidateRunRoot(); err != nil {
		return Config{}, err
	}
	return cfg.ResolveNetwork()
}
