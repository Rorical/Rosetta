package remote

import (
	"context"
	"fmt"
	"log/slog"
	"sync"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// Fee reads belong to the canonical producer, outside the preconfirmation
// writer. Preload the same calls loaders use so parsing and later search share
// a single hash-bound cache, including V4 slot0 reads used by other loaders.
func canonicalFees(ctx context.Context, cfg config.Config, client *rpc.Client, block rpc.ExecutionBlock, specs []bootstrap.Spec, previous ...[]pool.State) ([]pool.State, *rpc.ReadCache, error) {
	reader := &snapshot{client: client, block: block, chain: domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()}, deployments: cfg.Network.DEX, keepIdentity: true}
	defer reader.Close()
	tag := fmt.Sprintf("0x%x", block.Ref.Number)
	var states []pool.State
	var calls []pool.ContractCall
	local := make(map[domain.PoolKey]*pool.SlipstreamState)
	if len(previous) > 0 {
		for _, p := range previous[0] {
			local[p.Key] = p.Slipstream
		}
	}
	for _, spec := range specs {
		if !pipeline.DynamicFee(spec.Kind) {
			continue
		}
		p := pool.State{Key: domain.PoolKey{Address: spec.Address, Kind: spec.Kind, PoolID: spec.PoolID, Hooks: spec.Hooks}, Token0: spec.Token0, Token1: spec.Token1, Fee: spec.Fee, TickSpacing: spec.TickSpacing, Velodrome: spec.Velodrome}
		if m := local[p.Key]; m != nil && !m.NeedsRefresh {
			continue
		}
		to, data := spec.Address, selector("fee()")
		switch spec.Kind {
		case domain.PoolKindVelodromeV2:
			request, err := velodromev2.FeeCall(p, tag)
			if err != nil {
				return nil, nil, err
			}
			to, data = request.To, request.Data
		case domain.PoolKindCamelotV3:
			data = selector("globalState()")
		case domain.PoolKindV4:
			to = cfg.Network.DEX.V4StateView
			data = append(selector("getSlot0(bytes32)"), spec.PoolID[:]...)
		}
		states = append(states, p)
		calls = append(calls, pool.ContractCall{To: to, Data: data, Block: tag})
		countFeeRead(ctx, p.Key, local[p.Key])
	}
	for {
		_, err := reader.CallContracts(ctx, calls)
		if err == nil {
			break
		}
		if !retryFeeRead(ctx, err) {
			return nil, nil, err
		}
	}
	// Pools load concurrently: one pool after another, a chain's Slipstream
	// pools can take about 300 ms a block over a hosted RPC, longer than the
	// block itself, so the follower never catches up and the standard search
	// never runs. Their immutable identity reads are kept across blocks
	// (immutableReads); the bound stays under a typical provider RPS limit.
	failures := make([]error, len(states))
	slots := make(chan struct{}, feeParallelism)
	var wg sync.WaitGroup
	for i, p := range states {
		slots <- struct{}{}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-slots }()
			for {
				states[i], failures[i] = loadFee(ctx, reader, p, tag)
				if failures[i] == nil || !retryFeeRead(ctx, failures[i]) {
					return
				}
			}
		}()
	}
	wg.Wait()
	for _, err := range failures {
		if err != nil {
			return nil, nil, err
		}
	}
	return states, reader.readCache(), nil
}

// feeParallelism bounds concurrent per-pool fee loads in one block.
const feeParallelism = 4

// retryFeeRead reports whether a failed fee read should be sent again: a
// throttled read waits out the shared cooldown instead of stopping the
// follower.
func retryFeeRead(ctx context.Context, err error) bool {
	return ctx.Err() == nil && rpc.IsRateLimited(err)
}

func loadFee(ctx context.Context, reader *snapshot, p pool.State, tag string) (pool.State, error) {
	switch p.Key.Kind {
	case domain.PoolKindVelodromeV2:
		return velodromev2.LoadFee(ctx, reader, p, tag)
	case domain.PoolKindCamelotV3:
		return camelotv3.LoadFee(ctx, reader, p, tag)
	case domain.PoolKindSlipstream:
		return slipstream.LoadFee(ctx, reader, p, tag)
	case domain.PoolKindV4:
		return uniswapv4.LoadFee(ctx, reader, p, tag)
	}
	return p, nil
}

// countFeeRead attributes one canonical fee read. A Slipstream read means its
// local fee model needed a refresh, at several calls each; the logged reason
// names the event that invalidated it.
func countFeeRead(ctx context.Context, key domain.PoolKey, model *pool.SlipstreamState) {
	metrics := telemetry.FromContext(ctx)
	switch key.Kind {
	case domain.PoolKindSlipstream:
		metrics.Add(telemetry.Standard, telemetry.CanonicalFeeSlipstream, 1)
		reason := "no_model"
		if model != nil {
			reason = model.UnavailableReason
		}
		slog.Info("slipstream fee model refresh", "pool", key.Address.Hex(), "reason", reason)
	case domain.PoolKindV4:
		metrics.Add(telemetry.Standard, telemetry.CanonicalFeeV4, 1)
	default:
		metrics.Add(telemetry.Standard, telemetry.CanonicalFeeOther, 1)
	}
}
