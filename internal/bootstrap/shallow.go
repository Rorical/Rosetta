package bootstrap

import (
	"context"
	"fmt"
	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// LoadShallow loads protocol state without tick expansion at the caller's fixed snapshot.
func LoadShallow(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	switch spec.Kind {
	case domain.PoolKindVelodromeV2:
		return velodromev2.Load(ctx, caller, spec.Address, spec.Token0, spec.Token1, block)
	case domain.PoolKindV2:
		return uniswapv2.Load(ctx, caller, uniswapv2.Spec{Address: spec.Address, Token0: spec.Token0, Token1: spec.Token1, Fee: spec.Fee}, block)
	case domain.PoolKindCamelotV3:
		var output string
		if err := caller.CallContract(ctx, spec.Address, []byte{0xd0, 0xc9, 0x3a, 0x7c}, block, &output); err != nil {
			return pool.State{}, fmt.Errorf("read discovered Camelot spacing: %w", err)
		}
		spacing, err := camelotv3.ParseTickSpacing(output)
		if err != nil {
			return pool.State{}, err
		}
		return camelotv3.Load(ctx, caller, camelotv3.Spec{Address: spec.Address, Token0: spec.Token0, Token1: spec.Token1, Fee: spec.Fee, TickSpacing: spacing}, block)
	case domain.PoolKindV3, domain.PoolKindSlipstream:
		loaded, err := uniswapv3.LoadShallow(ctx, caller, uniswapv3.Spec{Address: spec.Address, Token0: spec.Token0, Token1: spec.Token1, Fee: spec.Fee, TickSpacing: spec.TickSpacing}, block)
		if err != nil {
			return pool.State{}, err
		}
		loaded.Key = spec.key()
		if spec.Kind == domain.PoolKindSlipstream {
			return slipstream.LoadFee(ctx, caller, loaded, block)
		}
		return loaded, nil
	case domain.PoolKindSolidlyV3:
		loaded, err := solidlyv3.LoadShallow(ctx, caller, solidlyv3.Spec{Address: spec.Address, Token0: spec.Token0, Token1: spec.Token1, TickSpacing: spec.TickSpacing}, block)
		if err != nil {
			return pool.State{}, err
		}
		loaded.Key = spec.key()
		return loaded, nil
	case domain.PoolKindV4:
		return uniswapv4.Load(ctx, caller, uniswapv4.Spec{PoolID: spec.PoolID, Token0: spec.Token0, Token1: spec.Token1, TickSpacing: spec.TickSpacing, Hooks: spec.Hooks}, block)
	default:
		return pool.State{}, fmt.Errorf("%w: discovered pool kind %d", domain.ErrUnsupported, spec.Kind)
	}
}
