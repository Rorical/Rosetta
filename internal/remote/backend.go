package remote

import (
	"context"
	"fmt"
	"time"

	"github.com/Rorical/Rosetta/internal/chain"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
)

// Factory creates dormant RPC runtimes without opening network or output resources.
type Factory struct{}
type backend struct{ config config.Config }

func (Factory) Create(ctx context.Context, cfg config.Config) (chain.Runtime, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	resolved, err := cfg.ResolveNetwork()
	if err != nil {
		return nil, err
	}
	if resolved.RPCURL == "" {
		return nil, fmt.Errorf("RPC backend requires -rpc")
	}
	if resolved.ReactiveConfig != "" && (resolved.SearchConfig == "" || resolved.ExecutionConfig != "") {
		return nil, fmt.Errorf("reactive execution requires search and a separate unconfirmed policy")
	}
	if resolved.ReactivePreflight && resolved.ReactiveConfig == "" {
		return nil, fmt.Errorf("reactive preflight requires -reactive-config")
	}
	sources := 0
	for _, source := range []string{resolved.PoolSnapshot, resolved.PoolConfig, resolved.PoolCatalog} {
		if source != "" {
			sources++
		}
	}
	if sources != 1 {
		return nil, fmt.Errorf("RPC backend requires exactly one of -pool-snapshot, -pool-config or -pool-catalog")
	}
	if resolved.RPCSearchRefresh == "" {
		resolved.RPCSearchRefresh = "canonical"
	}
	if resolved.RPCSearchRefresh != "canonical" && resolved.RPCSearchRefresh != "logs" {
		return nil, fmt.Errorf("invalid RPC search refresh mode %q", resolved.RPCSearchRefresh)
	}
	if resolved.RPCSearchRefresh == "logs" && resolved.ExecutionConfig != "" {
		return nil, fmt.Errorf("log-only search is restricted to candidate paper runs")
	}
	if resolved.RPCPoll == 0 {
		resolved.RPCPoll = 2 * time.Second
	}
	if resolved.DiscoveryConfig != "" && resolved.SearchConfig == "" {
		return nil, fmt.Errorf("discovery requires search configuration")
	}
	switch resolved.EventSource {
	case "", "auto":
		if resolved.FlashblocksURL != "" {
			resolved.EventSource = "flashblocks"
		} else {
			resolved.EventSource = "rpc"
		}
	case "rpc", "sequencer", "flashblocks":
	default:
		return nil, fmt.Errorf("invalid event source %q", resolved.EventSource)
	}
	if resolved.EventSource == "flashblocks" && (resolved.Network.Family != domain.FamilyOPStack || resolved.FlashblocksURL == "") {
		return nil, fmt.Errorf("Flashblocks requires an OP Stack network and -flashblocks URL")
	}
	if resolved.EventSource == "sequencer" && (resolved.Network.Family != domain.FamilyNitro || resolved.FeedURL == "") {
		return nil, fmt.Errorf("sequencer events require a Nitro feed")
	}
	if resolved.FlashblocksURL != "" && resolved.EventSource != "flashblocks" {
		return nil, fmt.Errorf("-flashblocks requires the flashblocks event source")
	}
	if resolved.FlashblocksReceiptsRPC && resolved.EventSource != "flashblocks" {
		return nil, fmt.Errorf("-flashblocks-receipts-rpc requires a Flashblocks source")
	}
	if resolved.FastURL != "" {
		if resolved.Network.Family != domain.FamilyNitro || resolved.FlashblocksURL != "" || resolved.FastFirstIndex < 0 || resolved.FastCanonicalFirstIndex < 0 {
			return nil, fmt.Errorf("Fast Feed requires Nitro and explicitly measured first-index mapping")
		}
	}
	if resolved.PlanOut != "" && resolved.ExecutionConfig == "" {
		return nil, fmt.Errorf("-plan-out requires -execution-config")
	}
	if resolved.ExecutionPaperConfig != "" && (resolved.Network.Family != domain.FamilyOPStack || resolved.SearchConfig == "" || resolved.ExecutionConfig != "") {
		return nil, fmt.Errorf("execution paper requires OP RPC search and is separate from dispatch confirmation")
	}
	if resolved.ExecutionConfig != "" && resolved.SearchConfig == "" {
		return nil, fmt.Errorf("-execution-config requires -search-config")
	}
	if (resolved.ResearchOut != "" || resolved.ResearchConfig != "") && resolved.SearchConfig == "" {
		return nil, fmt.Errorf("research requires search configuration")
	}
	if resolved.CandidateOut != "" && resolved.SearchConfig == "" {
		return nil, fmt.Errorf("-candidate-out requires -search-config")
	}
	if err := resolved.ValidateRunRoot(); err != nil {
		return nil, err
	}
	if resolved.Blocks < 0 || resolved.RPCPoll < 0 || resolved.RPCMinInterval < 0 {
		return nil, fmt.Errorf("negative block count or RPC interval")
	}
	return &backend{config: resolved}, nil
}
func (b *backend) Identity() domain.ChainIdentity {
	return domain.ChainIdentity{Family: b.config.Network.Family, ChainID: b.config.ChainID()}
}
func (b *backend) Run(ctx context.Context) error { return run(ctx, b.config) }
