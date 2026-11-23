package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

type auditView struct {
	view     state.View
	coverage coverageSet
}

func (v auditView) Pool(key domain.PoolKey) (pool.State, bool) {
	p, ok := v.view.Pool(key)
	if v.coverage.Pools != nil && !v.coverage.Pools[key] {
		return pool.State{}, false
	}
	if v.coverage.Tokens != nil && (!v.coverage.Tokens[p.Token0] || !v.coverage.Tokens[p.Token1]) {
		return pool.State{}, false
	}
	return p, ok
}
func (v auditView) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	if _, ok := v.Pool(key); !ok {
		return pool.DeepState{}, false
	}
	return v.view.Deep(key)
}
func replayAudit(ctx context.Context, input io.Reader, output *json.Encoder, cases caseSet, coverage coverageSet, policy pipeline.SearchPolicy, costs pipeline.ResearchPolicy, automatic ...*automaticAudit) error {
	var auto *automaticAudit
	if len(automatic) > 0 {
		auto = automatic[0]
	}
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 65536), 128<<20)
	var store *state.Store
	var topology *pipeline.Topology
	seen := map[string]bool{}
	reasons := map[string]int{}
	found := 0
	unknownCosts := 0
	line := 0
	report := func(view state.View, ref domain.BlockRef, before uint64, dirty []domain.PoolKey) error {
		if auto != nil {
			if err := auto.observe(ctx, view, topology, ref, before, dirty); err != nil {
				return err
			}
		}
		for _, known := range cases.Cases {
			if seen[known.ID] || known.BlockHash != ref.Hash || known.BeforeTx != before {
				continue
			}
			diagnostic, err := backtest.Diagnose(ctx, auditView{view, coverage}, topology.Table, topology.Engine, known, backtest.DiagnosticOptions{Diverse: true, Model: dex.RankingModel, Depth: policy.Depth, Truncated: topology.Truncated, ActiveTokens: coverage.Tokens, Dirty: dirty, Bounds: policy.Bounds, SwapFee: dex.SwapFee, Costs: func(token domain.Address, input, gross *big.Int) (*big.Int, string) {
				cost := costs.Economics(token, input, gross)
				return cost.Net, cost.Basis
			}})
			if err != nil {
				return err
			}
			if diagnostic.Found {
				found++
			}
			if diagnostic.CostAdjusted == nil {
				unknownCosts++
			}
			for _, reason := range diagnostic.Reasons {
				reasons[reason]++
			}
			seen[known.ID] = true
			if err := output.Encode(map[string]any{"type": "diagnostic", "chain_id": cases.ChainID, "block_hash": ref.Hash.Hex(), "before_tx": before, "result": diagnostic}); err != nil {
				return err
			}
		}
		return nil
	}
	for scanner.Scan() {
		line++
		if err := ctx.Err(); err != nil {
			return err
		}
		var record backtest.Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return fmt.Errorf("capture line %d: %w", line, err)
		}
		if record.ChainID != cases.ChainID {
			return fmt.Errorf("capture chain differs at line %d", line)
		}
		switch record.Type {
		case "snapshot":
			if auto != nil {
				auto.rememberAnchor(record.Block)
			}
			store = state.New()
			if err := store.LoadSnapshot(record.Snapshot); err != nil {
				return err
			}
			if store.Block() != record.Block {
				return fmt.Errorf("snapshot identity differs at line %d", line)
			}
			pools := []pool.State{}
			view := auditView{store.View(), coverage}
			for _, p := range store.Pools() {
				if _, ok := view.Pool(p.Key); ok {
					pools = append(pools, p)
				}
			}
			var err error
			topology, err = pipeline.BuildTopology(ctx, pools, pipeline.TopologyOptions{Depth: policy.Depth, MaxCycles: policy.MaxCycles, K: policy.K, ExplorePaths: policy.ExplorePaths, Notional: policy.Notional})
			if err != nil {
				return err
			}
		case "dependencies":
			if store == nil || store.Block() != record.Block {
				return fmt.Errorf("dependency capture has a different anchor")
			}
			for _, p := range record.Pools {
				if _, ok := store.Pool(p.Key); !ok {
					return fmt.Errorf("dependency capture adds an unknown pool")
				}
				if err := store.Upsert(p); err != nil {
					return err
				}
			}
			for _, d := range record.Deep {
				if d.State.LoadedBlock != record.Block {
					return fmt.Errorf("tick capture has a different anchor")
				}
				if err := store.SetDeep(d.Key, d.State); err != nil {
					return err
				}
			}
			store.ClearDirty()
		case "block":
			if store == nil {
				return fmt.Errorf("capture begins without pool snapshot")
			}
			if auto != nil {
				auto.startBlock(record)
			}
			branch, err := ingest.NewTransactionBranch(store)
			if err != nil {
				return err
			}
			if err := branch.Begin(record.Block); err != nil {
				return fmt.Errorf("capture line %d: %w", line, err)
			}
			if err := report(store.View(), record.Block, 0, store.View().PoolKeys()); err != nil {
				return err
			}
			hashes := make([]domain.Hash, len(record.Transactions))
			for i, tx := range record.Transactions {
				dirty, err := branch.Apply(ctx, tx)
				if err != nil {
					return err
				}
				hashes[i] = tx.Hash
				if err := report(branch.Working().View(), record.Block, uint64(i+1), append(dirty, topology.DynamicPools...)); err != nil {
					return err
				}
			}
			if err := branch.Commit(record.Block, hashes); err != nil {
				return err
			}
			if auto != nil {
				if err := auto.endBlock(ctx); err != nil {
					return err
				}
			}
			store = branch.Base()
		default:
			return fmt.Errorf("unknown capture record type %q", record.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if auto != nil {
		return auto.summary()
	}
	for _, known := range cases.Cases {
		if !seen[known.ID] {
			reasons["position_not_captured"]++
			if err := output.Encode(map[string]any{"type": "diagnostic", "chain_id": cases.ChainID, "result": backtest.Diagnostic{ID: known.ID, Reasons: []string{"position_not_captured"}, CostBasis: "unknown"}}); err != nil {
				return err
			}
		}
	}
	return output.Encode(map[string]any{"type": "summary", "chain_id": cases.ChainID, "total": len(cases.Cases), "positions_replayed": len(seen), "paths_found": found, "unknown_costs": unknownCosts, "reasons": reasons})
}
