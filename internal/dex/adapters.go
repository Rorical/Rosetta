package dex

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/dex/camelotv3"
	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/dex/wrap"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// NewAdapter selects the pool math implementation for a registered protocol.
func NewAdapter(p pool.State) (pool.Adapter, error) {
	switch p.Key.Kind {
	case domain.PoolKindV2:
		return uniswapv2.New(p.Fee)
	case domain.PoolKindVelodromeV2:
		return velodromev2.New(), nil
	case domain.PoolKindV3:
		return uniswapv3.New(p.Fee, p.TickSpacing)
	case domain.PoolKindCamelotV3:
		return camelotv3.New(p.Fee, p.TickSpacing)
	case domain.PoolKindSlipstream:
		return slipstream.New(p.Fee, p.TickSpacing)
	case domain.PoolKindSolidlyV3:
		return solidlyv3.New(p.TickSpacing)
	case domain.PoolKindV4:
		return uniswapv4.New()
	case domain.PoolKindMetricOmm:
		return metricomm.New(), nil
	case domain.PoolKindWrap:
		return wrap.New(), nil
	default:
		return nil, fmt.Errorf("%w: pool kind %d", domain.ErrUnsupported, p.Key.Kind)
	}
}

// SwapFee gives coarse ranking the same fee units and direction as exact pool
// quotes. Cached non-log dependencies still require a local state refresh.
func SwapFee(p pool.State, tokenIn domain.Address) (uint32, error) {
	if tokenIn != p.Token0 && tokenIn != p.Token1 {
		return 0, fmt.Errorf("%w: input token outside pool", pool.ErrInvalidState)
	}
	if p.Key.Kind == domain.PoolKindV4 {
		if p.Key.Hooks != (domain.Address{}) {
			return 0, uniswapv4.ErrUnsupportedHooks
		}
		return uniswapv4.SwapFee(p.Fee, p.ProtocolFee, tokenIn == p.Token0)
	}
	if p.Key.Kind == domain.PoolKindSlipstream {
		return slipstream.LocalFee(p)
	}
	if p.Key.Kind == domain.PoolKindMetricOmm {
		return metricomm.SwapFee(p, tokenIn)
	}
	if p.Key.Kind == domain.PoolKindWrap {
		// A deposit or withdrawal is 1:1.
		return 0, wrap.Validate(p)
	}
	fee := p.Fee
	if p.Key.Kind == domain.PoolKindCamelotV3 && tokenIn == p.Token1 && p.FeeOneForZero != nil {
		fee = *p.FeeOneForZero
	}
	if fee >= 1_000_000 {
		return 0, fmt.Errorf("%w: fee outside [0, 1000000)", pool.ErrInvalidState)
	}
	switch p.Key.Kind {
	case domain.PoolKindV2, domain.PoolKindV3, domain.PoolKindCamelotV3, domain.PoolKindSlipstream, domain.PoolKindVelodromeV2, domain.PoolKindSolidlyV3:
		return fee, nil
	default:
		return 0, fmt.Errorf("%w: pool kind %d", domain.ErrUnsupported, p.Key.Kind)
	}
}
