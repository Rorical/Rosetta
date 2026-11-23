package dex

import (
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/dex/wrap"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestSwapFeeMatchesV4DirectionalAdapterFees(t *testing.T) {
	p := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV4}, Token0: domain.Address{1}, Token1: domain.Address{2}, Fee: 3000, ProtocolFee: 100 | 1000<<12}
	for _, token := range []domain.Address{p.Token0, p.Token1} {
		got, err := SwapFee(p, token)
		want, referenceErr := uniswapv4.SwapFee(p.Fee, p.ProtocolFee, token == p.Token0)
		if err != nil || referenceErr != nil || got != want {
			t.Fatalf("directional fee=%d want=%d error=%v", got, want, err)
		}
	}
	p.Key.Hooks = domain.Address{3}
	if _, err := SwapFee(p, p.Token0); !errors.Is(err, uniswapv4.ErrUnsupportedHooks) {
		t.Fatal("hook-dependent pool received a static fee estimate")
	}
}

func TestCoarseCamelotFeeUsesTradeDirection(t *testing.T) {
	reverse := uint32(0)
	p := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindCamelotV3}, Token0: domain.Address{1}, Token1: domain.Address{2}, Fee: 500, FeeOneForZero: &reverse}
	forward, err := SwapFee(p, p.Token0)
	if err != nil || forward != 500 {
		t.Fatal("wrong forward fee")
	}
	backward, err := SwapFee(p, p.Token1)
	if err != nil || backward != 0 {
		t.Fatal("reverse zero fee replaced by forward fee")
	}
}

// MetricOmm prices off an oracle quote and per-bin fees. Its adapter needs no
// fee; coarse ranking takes the fee from the pool's words, and has neither a
// fee nor a model for a pool whose words were not read.
func TestMetricOmmHasAnAdapterAndNoFeeWithoutState(t *testing.T) {
	p := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindMetricOmm}, Token0: domain.Address{1}, Token1: domain.Address{2}}
	adapter, err := NewAdapter(p)
	if err != nil || adapter.Kind() != domain.PoolKindMetricOmm {
		t.Fatalf("adapter %v, error %v", adapter, err)
	}
	if _, err := SwapFee(p, p.Token0); !errors.Is(err, pool.ErrInvalidState) {
		t.Fatalf("fee of an unread pool: %v", err)
	}
	if _, err := RankingModel(p); !errors.Is(err, pool.ErrInvalidState) {
		t.Fatalf("model of an unread pool: %v", err)
	}
}

// The wrap of the native token is registered like a protocol: an identity
// adapter, no fee in either direction and a flat screening model. A pool of
// the kind that is not that wrap has none of them.
func TestWrapHasAnAdapterNoFeeAndAFlatModel(t *testing.T) {
	p := wrap.State(domain.Address{7})
	adapter, err := NewAdapter(p)
	if err != nil || adapter.Kind() != domain.PoolKindWrap {
		t.Fatalf("adapter %v, error %v", adapter, err)
	}
	for _, token := range []domain.Address{p.Token0, p.Token1} {
		if fee, err := SwapFee(p, token); err != nil || fee != 0 {
			t.Fatalf("fee %d, error %v", fee, err)
		}
	}
	if _, err := SwapFee(p, domain.Address{9}); !errors.Is(err, pool.ErrInvalidState) {
		t.Fatalf("fee for a token outside the pool: %v", err)
	}
	model, err := RankingModel(p)
	if err != nil || model.Reserve0.Sign() <= 0 || model.Reserve0.Cmp(model.Reserve1) != 0 || model.ConstantProduct {
		t.Fatalf("model %+v, error %v", model, err)
	}
	p.Token1 = domain.Address{9}
	if _, err := SwapFee(p, p.Token0); !errors.Is(err, pool.ErrInvalidState) {
		t.Fatalf("fee of a malformed wrap: %v", err)
	}
	if _, err := RankingModel(p); !errors.Is(err, pool.ErrInvalidState) {
		t.Fatalf("model of a malformed wrap: %v", err)
	}
}
