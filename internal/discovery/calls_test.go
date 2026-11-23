package discovery

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestDiscoverLogSourceError(t *testing.T) {
	logs := newFakeLogs()
	logs.err = errors.New("boom")

	discoverer, err := New(logs, newFakeCaller(), testConfig([]Source{SourceUniswapV3}, repeatedAddress(0xc0)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = discoverer.Discover(context.Background(), 1, 2)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("error = %v, want the log source failure", err)
	}
}

// TestDiscoverTransportErrorsAbort pins the classification: a plain error from
// a caller means the request failed on the way, so it aborts instead of being
// absorbed per pool. Reverts are *rpc.RPCError and covered in resilience_test.
func TestDiscoverTransportErrorsAbort(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	v3Pool := repeatedAddress(0x31)
	slipstreamPool := repeatedAddress(0x32)

	tests := []struct {
		name  string
		kind  domain.PoolKind
		pool  domain.Address
		setup func(*fakeCaller)
	}{
		{
			name: "liquidity",
			kind: domain.PoolKindV3,
			pool: v3Pool,
			setup: func(c *fakeCaller) {
				c.setError(v3Pool, liquidityCall, errors.New("liquidity unavailable"))
			},
		},
		{
			name: "slipstream fee",
			kind: domain.PoolKindSlipstream,
			pool: slipstreamPool,
			setup: func(c *fakeCaller) {
				c.setError(slipstreamPool, feeCall, errors.New("fee unavailable"))
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := newFakeLogs()
			caller := newFakeCaller()
			test.setup(caller)

			var config Config
			if test.kind == domain.PoolKindSlipstream {
				logs.add(slipstreamLog(quote0, quote1, 1, test.pool))
				config = testConfig([]Source{SourceSlipstream}, quote0, quote1)
			} else {
				logs.add(v3Log(quote0, quote1, 500, 10, test.pool))
				config = testConfig([]Source{SourceUniswapV3}, quote0, quote1)
			}
			discoverer, err := New(logs, caller, config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = discoverer.Discover(context.Background(), 1, 2)
			if err == nil {
				t.Fatal("Discover succeeded, want call error")
			}
		})
	}
}

func TestDiscoverContextCanceled(t *testing.T) {
	logs := newFakeLogs()
	caller := newFakeCaller()
	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV3}, repeatedAddress(0xc0)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = discoverer.Discover(ctx, 1, 2)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(logs.filters) != 0 || len(caller.calls) != 0 {
		t.Fatalf("filters/calls = %d/%d, want none", len(logs.filters), len(caller.calls))
	}
}

func TestDiscoverReversedRange(t *testing.T) {
	logs := newFakeLogs()
	discoverer, err := New(logs, newFakeCaller(), testConfig([]Source{SourceUniswapV3}, repeatedAddress(0xc0)))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := discoverer.Discover(context.Background(), 5, 4); err == nil {
		t.Fatal("Discover succeeded, want range error")
	}
	if len(logs.filters) != 0 {
		t.Fatalf("filters = %d, want none", len(logs.filters))
	}
}

func TestDiscoverBatchedRounds(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	v3Pool := repeatedAddress(0x31)
	slipstreamPool := repeatedAddress(0x32)

	build := func(caller pool.ContractCaller) Result {
		logs := newFakeLogs()
		logs.add(
			v3Log(quote0, quote1, 500, 10, v3Pool),
			slipstreamLog(quote0, quote1, 1, slipstreamPool),
		)
		discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV3, SourceSlipstream}, quote0, quote1))
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		result, err := discoverer.Discover(context.Background(), 1, 2)
		if err != nil {
			t.Fatalf("Discover: %v", err)
		}
		return result
	}

	batchCaller := &fakeBatchCaller{fakeCaller: newFakeCaller()}
	batchCaller.setFee(slipstreamPool, 377)
	batchCaller.setLiquidity(Spec{Address: v3Pool, Kind: domain.PoolKindV3}, big.NewInt(10))
	batchCaller.setLiquidity(Spec{Address: slipstreamPool, Kind: domain.PoolKindSlipstream}, big.NewInt(20))
	batched := build(batchCaller)

	if len(batchCaller.batches) != 2 {
		t.Fatalf("batches = %d, want 2 (fees then liquidity)", len(batchCaller.batches))
	}
	if len(batchCaller.batches[0]) != 1 || len(batchCaller.batches[1]) != 2 {
		t.Fatalf("batch sizes = %d/%d, want 1/2", len(batchCaller.batches[0]), len(batchCaller.batches[1]))
	}

	sequentialCaller := newFakeCaller()
	sequentialCaller.setFee(slipstreamPool, 377)
	sequentialCaller.setLiquidity(Spec{Address: v3Pool, Kind: domain.PoolKindV3}, big.NewInt(10))
	sequentialCaller.setLiquidity(Spec{Address: slipstreamPool, Kind: domain.PoolKindSlipstream}, big.NewInt(20))
	sequential := build(sequentialCaller)

	if !reflect.DeepEqual(batched, sequential) {
		t.Fatalf("batched and sequential results differ:\n%+v\n%+v", batched, sequential)
	}
}

func TestDiscoverV4LiquidityUsesStateView(t *testing.T) {
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	log := v4Log(t, token0, token1, 500, 10, domain.Address{})

	logs := newFakeLogs()
	logs.add(log)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: v4PoolManager, Kind: domain.PoolKindV4, PoolID: log.Topics[1]}, big.NewInt(5))

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV4}, token0, token1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := discoverer.Discover(context.Background(), 1, 2); err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(caller.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(caller.calls))
	}
	call := caller.calls[0]
	if call.To != stateView {
		t.Fatalf("call target = %s, want StateView %s", call.To, stateView)
	}
	want := append(append([]byte(nil), getLiquidityCall...), log.Topics[1][:]...)
	if !reflect.DeepEqual(call.Data, want) {
		t.Fatalf("call data = %x, want %x", call.Data, want)
	}
}

func TestNewValidation(t *testing.T) {
	validConfig := testConfig([]Source{SourceUniswapV3}, repeatedAddress(0xc0))

	if _, err := New(nil, newFakeCaller(), validConfig); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil logs error = %v, want ErrInvalidConfig", err)
	}
	if _, err := New(newFakeLogs(), nil, validConfig); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("nil caller error = %v, want ErrInvalidConfig", err)
	}

	noQuotes := validConfig
	noQuotes.QuoteTokens = nil
	if _, err := New(newFakeLogs(), newFakeCaller(), noQuotes); err != nil {
		t.Fatalf("no quotes error = %v, want global discovery to validate", err)
	}

	noSources := validConfig
	noSources.Sources = nil
	if _, err := New(newFakeLogs(), newFakeCaller(), noSources); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("no sources error = %v, want ErrInvalidConfig", err)
	}

	negativeFloor := validConfig
	negativeFloor.MinLiquidity = big.NewInt(-1)
	if _, err := New(newFakeLogs(), newFakeCaller(), negativeFloor); !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("negative floor error = %v, want ErrInvalidConfig", err)
	}

	discoverer, err := New(newFakeLogs(), newFakeCaller(), validConfig)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if discoverer.Config.MinLiquidity == nil {
		t.Fatal("New left MinLiquidity nil")
	}
}
