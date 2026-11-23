package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/discovery"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/state"
)

func loadMetadata(cfg options) (uint64, []backtest.PoolMetadata, map[domain.Hash]bool, error) {
	var chain uint64
	headers := map[domain.Hash]domain.BlockRef{}
	var head domain.BlockRef
	metadata := map[domain.PoolKey]backtest.PoolMetadata{}
	err := scanJSONL(cfg.capture, func(raw []byte) error {
		var record backtest.Record
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if chain == 0 {
			chain = record.ChainID
		}
		if chain == 0 || record.ChainID != chain {
			return fmt.Errorf("capture chain mismatch")
		}
		if record.Type == "snapshot" || record.Type == "block" {
			headers[record.Block.Hash] = record.Block
			head = record.Block
		}
		if record.Type != "snapshot" {
			return nil
		}
		store := state.New()
		if err := store.LoadSnapshot(record.Snapshot); err != nil {
			return err
		}
		for _, p := range store.Pools() {
			if old, ok := metadata[p.Key]; ok && (old.Token0 != p.Token0 || old.Token1 != p.Token1) {
				return fmt.Errorf("conflicting captured pool currencies")
			}
			metadata[p.Key] = backtest.PoolMetadata{Key: p.Key, Token0: p.Token0, Token1: p.Token1}
		}
		return nil
	})
	if err != nil {
		return 0, nil, nil, err
	}
	if cfg.inventory != "" {
		f, err := os.Open(cfg.inventory)
		if err != nil {
			return 0, nil, nil, err
		}
		defer f.Close()
		var index struct {
			ChainID uint64
			Records []discovery.Creation
		}
		decoder := json.NewDecoder(io.LimitReader(f, 128<<20))
		if err = decoder.Decode(&index); err != nil {
			return 0, nil, nil, err
		}
		if index.ChainID != chain {
			return 0, nil, nil, fmt.Errorf("inventory chain mismatch")
		}
		for _, c := range index.Records {
			key := c.Spec.PoolKey()
			m := backtest.PoolMetadata{Key: key, Token0: c.Spec.Token0, Token1: c.Spec.Token1, Created: c.Block}
			if old, ok := metadata[key]; ok && (old.Token0 != m.Token0 || old.Token1 != m.Token1) {
				return 0, nil, nil, fmt.Errorf("conflicting pool currencies")
			}
			metadata[key] = m
		}
	}
	if chain == 0 || len(metadata) == 0 {
		return 0, nil, nil, fmt.Errorf("capture has no pool snapshot")
	}
	result := make([]backtest.PoolMetadata, 0, len(metadata))
	for _, m := range metadata {
		result = append(result, m)
	}
	return chain, result, retainedChain(head, headers), nil
}
func runAutomatic(ctx context.Context, cfg options, stdout io.Writer, policy pipeline.SearchPolicy, costs pipeline.ResearchPolicy) error {
	chain, metadata, canonical, err := loadMetadata(cfg)
	if err != nil {
		return err
	}
	trace, err := loadTrace(cfg.paper, cfg.candidates, chain)
	if err != nil {
		return err
	}
	execution, err := loadExecutionTrace(cfg.executionPaper, chain)
	if err != nil {
		return err
	}
	var reader io.Reader // The scanner helper already validates gzip integrity on the metadata pass.
	f, err := openCapture(cfg.capture)
	if err != nil {
		return err
	}
	defer f.Close()
	reader = f
	output := stdout
	if cfg.output != "" {
		out, err := os.OpenFile(cfg.output, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		defer out.Close()
		output = out
	}
	encoder := json.NewEncoder(output)
	auto := &automaticAudit{execution: execution, metadata: metadata, canonical: canonical, trace: trace, output: encoder, policy: policy, costs: costs, limit: cfg.maxCases, chain: chain, counts: map[string]int{}, excluded: map[string]int{}, gross: map[string]*assetRecall{}}
	return replayAudit(ctx, reader, encoder, caseSet{ChainID: chain}, coverageSet{}, policy, costs, auto)
}

// Only the retained head's captured ancestors enter the competitor denominator.
// Missing ancestry remains excluded rather than treating an orphan as a fill.
func retainedChain(head domain.BlockRef, headers map[domain.Hash]domain.BlockRef) map[domain.Hash]bool {
	selected := map[domain.Hash]bool{}
	for {
		if selected[head.Hash] {
			break
		}
		selected[head.Hash] = true
		parent, ok := headers[head.ParentHash]
		if !ok || parent.Number >= head.Number {
			break
		}
		head = parent
	}
	return selected
}
