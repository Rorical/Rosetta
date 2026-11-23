package catalog

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// V4Config bounds startup discovery to hookless keys in an explicit tier set.
// A manager has no enumerable token-pair lookup; deriving IDs avoids history scans.
type V4Config struct {
	Manager, StateView domain.Address
	Tiers              []V4Tier
}
type V4Tier struct {
	Fee         uint32 `json:"fee"`
	TickSpacing int32  `json:"tick_spacing"`
}
type v4JSON struct {
	Manager   string   `json:"pool_manager"`
	StateView string   `json:"state_view"`
	Tiers     []V4Tier `json:"tiers"`
}

func (v v4JSON) decode() (*V4Config, error) {
	manager, err := domain.ParseAddress(v.Manager)
	if err != nil {
		return nil, err
	}
	view, err := domain.ParseAddress(v.StateView)
	if err != nil {
		return nil, err
	}
	return &V4Config{Manager: manager, StateView: view, Tiers: v.Tiers}, nil
}
func (v V4Config) validate() error {
	if v.Manager == (domain.Address{}) || v.StateView == (domain.Address{}) || len(v.Tiers) == 0 || len(v.Tiers) > 32 {
		return fmt.Errorf("catalog: invalid V4 deployment or tiers")
	}
	seen := make(map[V4Tier]bool)
	for _, tier := range v.Tiers {
		if tier.Fee >= 1_000_000 || tier.TickSpacing <= 0 || tier.TickSpacing > 32767 || seen[tier] {
			return fmt.Errorf("catalog: invalid or duplicate V4 tier")
		}
		seen[tier] = true
	}
	return nil
}
func resolveV4(ctx context.Context, reader execution.StateReader, cfg V4Config, tokens []domain.Address) ([]bootstrap.Spec, error) {
	deployment, ok := reader.(uniswapv4.DeploymentProvider)
	if !ok {
		return nil, fmt.Errorf("catalog: V4 reader must bind its deployment")
	}
	manager, view := deployment.V4Deployment()
	if manager != cfg.Manager || view != cfg.StateView {
		return nil, fmt.Errorf("catalog: V4 deployment differs from network")
	}
	for _, address := range []domain.Address{manager, view} {
		if err := checkCode(ctx, reader, address); err != nil {
			return nil, err
		}
	}
	actual, err := readAddress(ctx, reader, view, selector("poolManager()"))
	if err != nil {
		return nil, err
	}
	if actual != manager {
		return nil, fmt.Errorf("catalog: StateView points to another manager")
	}
	var pools []bootstrap.Spec
	for i, token0 := range tokens {
		for _, token1 := range tokens[i+1:] {
			for _, tier := range cfg.Tiers {
				id, err := uniswapv4.PoolID(token0, token1, tier.Fee, tier.TickSpacing, domain.Address{})
				if err != nil {
					return nil, err
				}
				data := append(selector("getSlot0(bytes32)"), id[:]...)
				raw, err := reader.ReadContract(ctx, view, data)
				if err != nil {
					return nil, err
				}
				if len(raw) != 128 {
					return nil, fmt.Errorf("catalog: invalid V4 slot0 ABI")
				}
				price := new(big.Int).SetBytes(raw[:32])
				if price.Sign() == 0 {
					continue
				}
				if price.BitLen() > 160 || new(big.Int).SetBytes(raw[96:]).Cmp(new(big.Int).SetUint64(uint64(tier.Fee))) != 0 {
					return nil, fmt.Errorf("catalog: V4 static key/fee mismatch")
				}
				liquidity, err := readWord(ctx, reader, view, append(selector("getLiquidity(bytes32)"), id[:]...))
				if err != nil {
					return nil, err
				}
				if liquidity.BitLen() > 128 {
					return nil, fmt.Errorf("catalog: invalid V4 liquidity")
				}
				if liquidity.Sign() == 0 {
					continue
				}
				pools = append(pools, bootstrap.Spec{Address: manager, Kind: domain.PoolKindV4, PoolID: id, Token0: token0, Token1: token1, Fee: tier.Fee, TickSpacing: tier.TickSpacing})
			}
		}
	}
	return pools, nil
}

// VerifyV4Pool checks one event-derived static key without enumerating a universe.
func VerifyV4Pool(ctx context.Context, reader execution.StateReader, spec bootstrap.Spec, stateView domain.Address) (bootstrap.Spec, bool, error) {
	cfg := V4Config{Manager: spec.Address, StateView: stateView, Tiers: []V4Tier{{Fee: spec.Fee, TickSpacing: spec.TickSpacing}}}
	if spec.Hooks != (domain.Address{}) {
		return spec, false, fmt.Errorf("catalog: hooked V4 pool is unsupported")
	}
	if err := cfg.validate(); err != nil {
		return spec, false, err
	}
	for _, token := range []domain.Address{spec.Token0, spec.Token1} {
		if token != (domain.Address{}) {
			if err := checkCode(ctx, reader, token); err != nil {
				return spec, false, err
			}
		}
	}
	pools, err := resolveV4(ctx, reader, cfg, []domain.Address{spec.Token0, spec.Token1})
	if err != nil || len(pools) == 0 {
		return spec, false, err
	}
	if pools[0].PoolID != spec.PoolID {
		return spec, false, fmt.Errorf("catalog: event key differs from derived V4 ID")
	}
	return pools[0], true, nil
}
