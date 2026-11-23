package discovery

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// revert is the shape a node returns for a reverting eth_call: a JSON-RPC
// error, not a transport failure.
var revert = &rpc.RPCError{Code: -32000, Message: "execution reverted"}

func TestDiscoverLiquidityRevertSkipsOnlyThatPool(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	first := repeatedAddress(0x31)
	bad := repeatedAddress(0x32)
	third := repeatedAddress(0x33)

	tests := []struct {
		name      string
		newCaller func(*fakeCaller) pool.ContractCaller
	}{
		{name: "sequential", newCaller: func(f *fakeCaller) pool.ContractCaller { return f }},
		{name: "batch fallback", newCaller: func(f *fakeCaller) pool.ContractCaller {
			return &fakeBatchCaller{fakeCaller: f}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := newFakeCaller()
			base.setLiquidity(Spec{Address: first, Kind: domain.PoolKindV3}, big.NewInt(10))
			base.setError(bad, liquidityCall, revert)
			base.setLiquidity(Spec{Address: third, Kind: domain.PoolKindV3}, big.NewInt(30))

			logs := newFakeLogs()
			logs.add(
				v3Log(quote0, quote1, 500, 10, first),
				v3Log(quote0, quote1, 500, 10, bad),
				v3Log(quote0, quote1, 500, 10, third),
			)

			discoverer, err := New(logs, test.newCaller(base), testConfig([]Source{SourceUniswapV3}, quote0, quote1))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			result, err := discoverer.Discover(context.Background(), 1, 2)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(result.Pools) != 2 || result.Pools[0].Address != third || result.Pools[1].Address != first {
				t.Fatalf("pools = %v, want the two healthy pools ranked by liquidity", result.Pools)
			}
			if result.Stats.SkippedLiquidity != 1 || result.Stats.Selected != 2 {
				t.Fatalf("stats = %+v, want one liquidity skip and two selected", result.Stats)
			}
		})
	}
}

func TestDiscoverSlipstreamFeeRevertSkipsOnlyThatPool(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	first := repeatedAddress(0x41)
	bad := repeatedAddress(0x42)
	third := repeatedAddress(0x43)

	tests := []struct {
		name      string
		newCaller func(*fakeCaller) pool.ContractCaller
	}{
		{name: "sequential", newCaller: func(f *fakeCaller) pool.ContractCaller { return f }},
		{name: "batch fallback", newCaller: func(f *fakeCaller) pool.ContractCaller {
			return &fakeBatchCaller{fakeCaller: f}
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			base := newFakeCaller()
			base.setFee(first, 100)
			base.setError(bad, feeCall, revert)
			base.setFee(third, 300)
			base.setLiquidity(Spec{Address: first, Kind: domain.PoolKindSlipstream}, big.NewInt(10))
			base.setLiquidity(Spec{Address: third, Kind: domain.PoolKindSlipstream}, big.NewInt(20))

			logs := newFakeLogs()
			logs.add(
				slipstreamLog(quote0, quote1, 1, first),
				slipstreamLog(quote0, quote1, 1, bad),
				slipstreamLog(quote0, quote1, 1, third),
			)

			discoverer, err := New(logs, test.newCaller(base), testConfig([]Source{SourceSlipstream}, quote0, quote1))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			result, err := discoverer.Discover(context.Background(), 1, 2)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(result.Pools) != 2 || result.Pools[0].Address != third || result.Pools[1].Address != first {
				t.Fatalf("pools = %v, want the two priced pools", result.Pools)
			}
			if result.Pools[0].Fee != 300 || result.Pools[1].Fee != 100 {
				t.Fatalf("fees = %d/%d, want 300/100", result.Pools[0].Fee, result.Pools[1].Fee)
			}
			if result.Stats.SkippedInvalid != 1 || result.Stats.Selected != 2 {
				t.Fatalf("stats = %+v, want one invalid fee read and two selected", result.Stats)
			}
		})
	}
}
