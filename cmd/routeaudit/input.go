package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

type caseSet struct {
	ChainID uint64
	Cases   []backtest.KnownRoute
}
type poolJSON struct {
	Address string `json:"address"`
	Kind    string `json:"kind"`
	PoolID  string `json:"pool_id"`
	Hooks   string `json:"hooks"`
}
type coverageSet struct {
	Pools  map[domain.PoolKey]bool
	Tokens map[domain.Address]bool
}

func loadCases(path string) (caseSet, error) {
	var file struct {
		ChainID uint64 `json:"chain_id"`
		Cases   []struct {
			ID        string     `json:"id"`
			BlockHash string     `json:"block_hash"`
			BeforeTx  uint64     `json:"before_tx"`
			Start     string     `json:"start"`
			Input     string     `json:"input"`
			Pools     []poolJSON `json:"pools"`
			Tokens    []string   `json:"tokens"`
		} `json:"cases"`
	}
	if err := readJSON(path, &file); err != nil {
		return caseSet{}, err
	}
	if file.ChainID == 0 || len(file.Cases) == 0 || len(file.Cases) > 10000 {
		return caseSet{}, fmt.Errorf("invalid case chain or count")
	}
	out := caseSet{ChainID: file.ChainID}
	ids := map[string]bool{}
	for _, wire := range file.Cases {
		hash, err := domain.ParseHash(wire.BlockHash)
		if err != nil || hash == (domain.Hash{}) {
			return out, fmt.Errorf("invalid historical block hash")
		}
		start, err := domain.ParseAddress(wire.Start)
		if err != nil {
			return out, err
		}
		amount, ok := new(big.Int).SetString(wire.Input, 10)
		if !ok || amount.Sign() <= 0 || amount.BitLen() > 256 || wire.ID == "" || ids[wire.ID] || len(wire.Pools) < 2 || len(wire.Pools) > 16 {
			return out, fmt.Errorf("invalid historical route identity or input")
		}
		ids[wire.ID] = true
		known := backtest.KnownRoute{ID: wire.ID, BlockHash: hash, BeforeTx: wire.BeforeTx, Input: amount, Cycle: graph.Cycle{Start: start, Len: len(wire.Pools)}}
		seenPools := map[domain.PoolKey]bool{}
		for _, wirePool := range wire.Pools {
			key, err := wirePool.key()
			if err != nil {
				return out, err
			}
			if seenPools[key] {
				return out, fmt.Errorf("historical cycle repeats a pool")
			}
			seenPools[key] = true
			known.Cycle.Pools = append(known.Cycle.Pools, key)
		}
		for _, text := range wire.Tokens {
			token, err := domain.ParseAddress(text)
			if err != nil {
				return out, err
			}
			known.Tokens = append(known.Tokens, token)
		}
		out.Cases = append(out.Cases, known)
	}
	return out, nil
}
func loadCoverage(path string, chainID uint64) (coverageSet, error) {
	out := coverageSet{}
	if path == "" {
		return out, nil
	}
	var file struct {
		ChainID uint64     `json:"chain_id"`
		Pools   []poolJSON `json:"pools"`
		Tokens  []string   `json:"tokens"`
	}
	if err := readJSON(path, &file); err != nil {
		return out, err
	}
	if file.ChainID != chainID {
		return out, fmt.Errorf("coverage chain mismatch")
	}
	if file.Pools != nil {
		out.Pools = map[domain.PoolKey]bool{}
		for _, wire := range file.Pools {
			key, err := wire.key()
			if err != nil {
				return out, err
			}
			out.Pools[key] = true
		}
	}
	if file.Tokens != nil {
		out.Tokens = map[domain.Address]bool{}
		for _, text := range file.Tokens {
			token, err := domain.ParseAddress(text)
			if err != nil {
				return out, err
			}
			out.Tokens[token] = true
		}
	}
	return out, nil
}
func (p poolJSON) key() (domain.PoolKey, error) {
	address, err := domain.ParseAddress(p.Address)
	if err != nil || address == (domain.Address{}) {
		return domain.PoolKey{}, fmt.Errorf("invalid pool address")
	}
	key := domain.PoolKey{Address: address}
	switch p.Kind {
	case "v2":
		key.Kind = domain.PoolKindV2
	case "v3":
		key.Kind = domain.PoolKindV3
	case "v4":
		key.Kind = domain.PoolKindV4
	case "slipstream":
		key.Kind = domain.PoolKindSlipstream
	case "velodrome_v2":
		key.Kind = domain.PoolKindVelodromeV2
	case "camelot_v3":
		key.Kind = domain.PoolKindCamelotV3
	case "solidly_v3":
		key.Kind = domain.PoolKindSolidlyV3
	default:
		return key, fmt.Errorf("unsupported pool protocol %q", p.Kind)
	}
	if key.IsV4() {
		key.PoolID, err = domain.ParseHash(p.PoolID)
		if err != nil || key.PoolID == (domain.Hash{}) {
			return key, fmt.Errorf("V4 pool ID required")
		}
		if p.Hooks != "" {
			key.Hooks, err = domain.ParseAddress(p.Hooks)
		}
	} else if p.PoolID != "" || p.Hooks != "" {
		return key, fmt.Errorf("non-V4 pool has singleton fields")
	}
	return key, err
}
func readJSON(path string, target any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) > 16<<20 {
		return fmt.Errorf("audit JSON exceeds 16 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if decoder.Decode(new(any)) != io.EOF {
		return fmt.Errorf("trailing audit JSON")
	}
	return nil
}
