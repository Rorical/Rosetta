package remote

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/catalog"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func bootstrapPools(ctx context.Context, cfg config.Config, client *rpc.Client, radius int16) (*poolstate.Store, []bootstrap.Spec, rpc.ExecutionBlock, error) {
	store := poolstate.New()
	if cfg.PoolSnapshot != "" {
		data, err := os.ReadFile(cfg.PoolSnapshot)
		if err != nil {
			return nil, nil, rpc.ExecutionBlock{}, err
		}
		if err := store.LoadSnapshot(data); err != nil {
			return nil, nil, rpc.ExecutionBlock{}, err
		}
		block, err := client.ExecutionBlock(ctx, store.Block().Hash)
		if err != nil {
			return nil, nil, rpc.ExecutionBlock{}, err
		}
		canonical, err := client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", block.Ref.Number))
		if err != nil || canonical.Ref.Hash != block.Ref.Hash || block.Ref != store.Block() {
			return nil, nil, rpc.ExecutionBlock{}, fmt.Errorf("RPC pool snapshot is not at the selected chain's canonical block")
		}
		return store, specsFor(store.Pools()), block, nil
	}
	block, err := client.ExecutionBlock(ctx, "latest")
	if err != nil {
		return nil, nil, rpc.ExecutionBlock{}, err
	}
	reader := &snapshot{client: client, block: block, chain: domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()}, deployments: cfg.Network.DEX}
	defer reader.Close()
	var specs []bootstrap.Spec
	if cfg.PoolCatalog != "" {
		universe, err := catalog.Load(cfg.PoolCatalog)
		if err != nil {
			return nil, nil, rpc.ExecutionBlock{}, err
		}
		specs, err = catalog.Resolve(ctx, reader, universe)
		if err != nil {
			return nil, nil, rpc.ExecutionBlock{}, err
		}
	} else {
		specs, err = loadPoolSpecs(cfg.PoolConfig)
		if err != nil {
			return nil, nil, rpc.ExecutionBlock{}, err
		}
	}
	store, err = seedWithReader(ctx, client, reader, specs, block, radius)
	if err != nil {
		return nil, nil, block, err
	}
	return store, specsFor(store.Pools()), block, nil
}

func seed(ctx context.Context, cfg config.Config, client *rpc.Client, specs []bootstrap.Spec, block rpc.ExecutionBlock, radius int16) (*poolstate.Store, error) {
	reader := &snapshot{client: client, block: block, chain: domain.ChainIdentity{Family: cfg.Network.Family, ChainID: cfg.ChainID()}, deployments: cfg.Network.DEX}
	defer reader.Close()
	return seedWithReader(ctx, client, reader, specs, block, radius)
}

func seedWithReader(ctx context.Context, client *rpc.Client, reader *snapshot, specs []bootstrap.Spec, block rpc.ExecutionBlock, radius int16) (*poolstate.Store, error) {
	store := poolstate.New()
	if err := bootstrap.Seed(ctx, reader, store, specs, block.Ref, radius); err != nil {
		return nil, err
	}
	current, err := client.ExecutionBlock(ctx, fmt.Sprintf("0x%x", block.Ref.Number))
	if err != nil || current.Ref.Hash != block.Ref.Hash {
		return nil, fmt.Errorf("RPC bootstrap block ceased to be canonical")
	}
	store.ClearDirty()
	return store, nil
}

func specsFor(pools []pool.State) []bootstrap.Spec {
	specs := make([]bootstrap.Spec, len(pools))
	for i, p := range pools {
		specs[i] = bootstrap.Spec{Address: p.Key.Address, Kind: p.Key.Kind, PoolID: p.Key.PoolID, Hooks: p.Key.Hooks, Token0: p.Token0, Token1: p.Token1, Fee: p.Fee, TickSpacing: p.TickSpacing, Velodrome: p.Velodrome, Metric: p.Metric}
	}
	return specs
}

func loadPoolSpecs(path string) ([]bootstrap.Spec, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var document struct {
		Pools []struct {
			Address     string `json:"address"`
			Kind        string `json:"kind"`
			Token0      string `json:"token0"`
			Token1      string `json:"token1"`
			PoolID      string `json:"pool_id"`
			Hooks       string `json:"hooks"`
			Fee         uint32 `json:"fee"`
			TickSpacing int32  `json:"tick_spacing"`
		}
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("decode pool config: %w", err)
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("pool config contains trailing JSON")
	}
	if len(document.Pools) == 0 {
		return nil, fmt.Errorf("pool config is empty")
	}
	specs := make([]bootstrap.Spec, 0, len(document.Pools))
	for _, entry := range document.Pools {
		var spec bootstrap.Spec
		for _, field := range []struct {
			text string
			out  *domain.Address
		}{{entry.Address, &spec.Address}, {entry.Token0, &spec.Token0}, {entry.Token1, &spec.Token1}} {
			*field.out, err = domain.ParseAddress(field.text)
			if err != nil {
				return nil, err
			}
		}
		switch entry.Kind {
		case "velodrome_v2":
			spec.Kind = domain.PoolKindVelodromeV2
		case "v2":
			spec.Kind = domain.PoolKindV2
		case "v3":
			spec.Kind = domain.PoolKindV3
		case "v4":
			spec.Kind = domain.PoolKindV4
		case "slipstream":
			spec.Kind = domain.PoolKindSlipstream
		case "camelot_v3":
			spec.Kind = domain.PoolKindCamelotV3
		case "solidly_v3":
			spec.Kind = domain.PoolKindSolidlyV3
		default:
			return nil, fmt.Errorf("unsupported pool kind %q", entry.Kind)
		}
		spec.Fee, spec.TickSpacing = entry.Fee, entry.TickSpacing
		if entry.PoolID != "" {
			spec.PoolID, err = domain.ParseHash(entry.PoolID)
			if err != nil {
				return nil, err
			}
		}
		if entry.Hooks != "" {
			spec.Hooks, err = domain.ParseAddress(entry.Hooks)
			if err != nil {
				return nil, err
			}
		}
		specs = append(specs, spec)
	}
	return specs, nil
}

// writePoolSnapshot replaces path whole: a reader sees the previous snapshot
// or the new one, never a partial file. The bootstrap checkpoint and the
// shutdown snapshot share the path.
func writePoolSnapshot(path string, store *poolstate.Store) error {
	data, err := store.EncodeSnapshot()
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
