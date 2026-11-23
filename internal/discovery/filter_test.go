package discovery

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestDiscoverTokenFilter(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	outsider := repeatedAddress(0xee)
	poolA := repeatedAddress(0x31)
	poolB := repeatedAddress(0x32)
	poolC := repeatedAddress(0x33)

	logs := newFakeLogs()
	logs.add(
		v3Log(outsider, quote1, 500, 10, poolA), // token0 outside the set
		v3Log(quote0, outsider, 500, 10, poolB), // token1 outside the set
		v3Log(quote0, quote1, 500, 10, poolC),
	)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: poolC, Kind: domain.PoolKindV3}, big.NewInt(1))

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV3}, quote0, quote1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 1 || result.Pools[0].Address != poolC {
		t.Fatalf("pools = %v, want only the quoted pair", result.Pools)
	}
	if result.Stats.SkippedToken != 2 {
		t.Fatalf("skipped token = %d, want 2", result.Stats.SkippedToken)
	}
}

func TestDiscoverFeeFilter(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	cheap := repeatedAddress(0x31)
	pricey := repeatedAddress(0x32)

	logs := newFakeLogs()
	logs.add(
		v3Log(quote0, quote1, 500, 10, cheap),
		v3Log(quote0, quote1, 3000, 60, pricey),
	)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: cheap, Kind: domain.PoolKindV3}, big.NewInt(1))

	config := testConfig([]Source{SourceUniswapV3}, quote0, quote1)
	config.MaxFee = 500
	discoverer, err := New(logs, caller, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 1 || result.Pools[0].Address != cheap {
		t.Fatalf("pools = %v, want the pool at the fee cap", result.Pools)
	}
	if result.Stats.SkippedFee != 1 {
		t.Fatalf("skipped fee = %d, want 1", result.Stats.SkippedFee)
	}
}

func TestDiscoverHooksFilter(t *testing.T) {
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	hooks := repeatedAddress(0x99)

	logs := newFakeLogs()
	logs.add(v4Log(t, token0, token1, 500, 10, hooks))
	caller := newFakeCaller()

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV4}, token0, token1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 0 {
		t.Fatalf("pools = %v, want none", result.Pools)
	}
	if result.Stats.SkippedHooks != 1 || result.Stats.SkippedInvalid != 0 {
		t.Fatalf("stats = %+v, want one hooks skip", result.Stats)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("calls = %v, want no liquidity read for a hooked pool", caller.calls)
	}
}

func TestDiscoverV4IDMismatch(t *testing.T) {
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	wrong := repeatedAddress(0xaa)
	var wrongID domain.Hash
	copy(wrongID[:], wrong[:])

	logs := newFakeLogs()
	logs.add(v4LogWithID(token0, token1, 500, 10, domain.Address{}, wrongID))
	caller := newFakeCaller()

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV4}, token0, token1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 0 || result.Stats.SkippedInvalid != 1 {
		t.Fatalf("pools/stats = %v/%+v, want no pools and one invalid", result.Pools, result.Stats)
	}
}

func TestDiscoverInvalidPairs(t *testing.T) {
	token0 := repeatedAddress(0xc0)
	token1 := repeatedAddress(0xc1)
	var zero domain.Address
	poolAddress := repeatedAddress(0x31)

	tests := []struct {
		name string
		log  rpc.Log
	}{
		{"equal tokens", v3Log(token0, token0, 500, 10, poolAddress)},
		{"zero token1", v3Log(token0, zero, 500, 10, poolAddress)},
		{"zero token0 outside v4", v3Log(zero, token1, 500, 10, poolAddress)},
		{"zero tick spacing", v3Log(token0, token1, 500, 0, poolAddress)},
		{"zero v3 fee", v3Log(token0, token1, 0, 10, poolAddress)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			logs := newFakeLogs()
			logs.add(test.log)
			discoverer, err := New(logs, newFakeCaller(), testConfig([]Source{SourceUniswapV3}, token0, token1, zero))
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			result, err := discoverer.Discover(context.Background(), 1, 2)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(result.Pools) != 0 || result.Stats.SkippedInvalid != 1 {
				t.Fatalf("pools/stats = %v/%+v, want one invalid pool", result.Pools, result.Stats)
			}
		})
	}
}

func TestDiscoverSlipstreamFee(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	cheap := repeatedAddress(0x31)
	pricey := repeatedAddress(0x32)

	tests := []struct {
		name       string
		feeOutputs map[domain.Address]string
		wantPools  int
		wantFee    uint32
		wantSkip   int
	}{
		{
			name:       "fee below cap",
			feeOutputs: map[domain.Address]string{cheap: wordHex(big.NewInt(377))},
			wantPools:  1,
			wantFee:    377,
		},
		{
			name:       "fee above cap",
			feeOutputs: map[domain.Address]string{pricey: wordHex(big.NewInt(2000))},
			wantSkip:   1,
		},
		{
			name:       "zero fee",
			feeOutputs: map[domain.Address]string{cheap: wordHex(big.NewInt(0))},
			wantSkip:   1,
		},
		{
			name:       "malformed fee word",
			feeOutputs: map[domain.Address]string{cheap: "0x01"},
			wantSkip:   1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			poolAddress := cheap
			if _, ok := test.feeOutputs[pricey]; ok {
				poolAddress = pricey
			}
			logs := newFakeLogs()
			logs.add(slipstreamLog(quote0, quote1, 1, poolAddress))
			caller := newFakeCaller()
			for pool, output := range test.feeOutputs {
				caller.set(pool, feeCall, output)
			}
			if test.wantPools > 0 {
				caller.setLiquidity(Spec{Address: poolAddress, Kind: domain.PoolKindSlipstream}, big.NewInt(1))
			}

			config := testConfig([]Source{SourceSlipstream}, quote0, quote1)
			config.MaxFee = 500
			discoverer, err := New(logs, caller, config)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			result, err := discoverer.Discover(context.Background(), 1, 2)
			if err != nil {
				t.Fatalf("Discover: %v", err)
			}
			if len(result.Pools) != test.wantPools {
				t.Fatalf("len(pools) = %d, want %d", len(result.Pools), test.wantPools)
			}
			if test.wantPools > 0 && result.Pools[0].Fee != test.wantFee {
				t.Fatalf("fee = %d, want %d", result.Pools[0].Fee, test.wantFee)
			}
			if result.Stats.SkippedFee != test.wantSkip {
				t.Fatalf("skipped fee = %d, want %d", result.Stats.SkippedFee, test.wantSkip)
			}
		})
	}
}

func TestDiscoverMinLiquidity(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	low := repeatedAddress(0x31)
	atFloor := repeatedAddress(0x32)

	logs := newFakeLogs()
	logs.add(
		v3Log(quote0, quote1, 500, 10, low),
		v3Log(quote0, quote1, 500, 10, atFloor),
	)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: low, Kind: domain.PoolKindV3}, big.NewInt(99))
	caller.setLiquidity(Spec{Address: atFloor, Kind: domain.PoolKindV3}, big.NewInt(100))

	config := testConfig([]Source{SourceUniswapV3}, quote0, quote1)
	config.MinLiquidity = big.NewInt(100)
	discoverer, err := New(logs, caller, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 1 || result.Pools[0].Address != atFloor {
		t.Fatalf("pools = %v, want the pool at the floor", result.Pools)
	}
	if result.Stats.SkippedLiquidity != 1 {
		t.Fatalf("skipped liquidity = %d, want 1", result.Stats.SkippedLiquidity)
	}
}

func TestDiscoverMalformedLiquidity(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	poolAddress := repeatedAddress(0x31)

	logs := newFakeLogs()
	logs.add(v3Log(quote0, quote1, 500, 10, poolAddress))
	caller := newFakeCaller()
	caller.set(poolAddress, liquidityCall, "0x01")

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV3}, quote0, quote1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 0 || result.Stats.SkippedLiquidity != 1 {
		t.Fatalf("pools/stats = %v/%+v, want one liquidity skip", result.Pools, result.Stats)
	}
}

func TestDiscoverMalformedLogCounted(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	valid := repeatedAddress(0x31)
	broken := v3Log(quote0, quote1, 500, 10, repeatedAddress(0x32))
	broken.Data = broken.Data[:abiWordSize]

	logs := newFakeLogs()
	logs.add(broken, v3Log(quote0, quote1, 500, 10, valid))
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: valid, Kind: domain.PoolKindV3}, big.NewInt(1))

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV3}, quote0, quote1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 1 || result.Pools[0].Address != valid {
		t.Fatalf("pools = %v, want the valid pool", result.Pools)
	}
	if result.Stats.SkippedInvalid != 1 || result.Stats.Decoded != 1 {
		t.Fatalf("stats = %+v, want one invalid and one decoded", result.Stats)
	}
}

func TestDiscoverWrongEmitterCounted(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	impostor := repeatedAddress(0x88)
	log := v3Log(quote0, quote1, 500, 10, repeatedAddress(0x31))
	log.Address = impostor

	logs := newFakeLogs()
	// Return the log from a query for the real factory, as a broken node could.
	logs.logs[uniswapV3Factory] = []rpc.Log{log}

	discoverer, err := New(logs, newFakeCaller(), testConfig([]Source{SourceUniswapV3}, quote0, quote1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 0 || result.Stats.SkippedInvalid != 1 {
		t.Fatalf("pools/stats = %v/%+v, want one invalid emitter", result.Pools, result.Stats)
	}
}

func TestDiscoverGlobalQuoteTokens(t *testing.T) {
	token0 := repeatedAddress(0xde)
	token1 := repeatedAddress(0xad)
	poolAddress := repeatedAddress(0x31)

	logs := newFakeLogs()
	logs.add(v3Log(token0, token1, 500, 10, poolAddress))
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: poolAddress, Kind: domain.PoolKindV3}, big.NewInt(5))

	config, err := LoadConfig([]byte(`{"sources":["uniswap_v3"]}`))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if len(config.QuoteTokens) != 0 {
		t.Fatalf("quote tokens = %v, want none", config.QuoteTokens)
	}
	discoverer, err := New(logs, caller, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 1 || result.Pools[0].Address != poolAddress {
		t.Fatalf("pools = %v, want the unlisted pair", result.Pools)
	}
	if result.Stats.SkippedToken != 0 {
		t.Fatalf("skipped token = %d, want 0 without an allowlist", result.Stats.SkippedToken)
	}
}

func TestDiscoverZeroLiquidityAlwaysSkipped(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	empty := repeatedAddress(0x31)
	live := repeatedAddress(0x32)

	logs := newFakeLogs()
	logs.add(
		v3Log(quote0, quote1, 500, 10, empty),
		v3Log(quote0, quote1, 500, 10, live),
	)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: empty, Kind: domain.PoolKindV3}, big.NewInt(0))
	caller.setLiquidity(Spec{Address: live, Kind: domain.PoolKindV3}, big.NewInt(5))

	config := testConfig([]Source{SourceUniswapV3}, quote0, quote1)
	config.MinLiquidity = big.NewInt(0)
	discoverer, err := New(logs, caller, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 1 || result.Pools[0].Address != live {
		t.Fatalf("pools = %v, want only the pool with active liquidity", result.Pools)
	}
	if result.Stats.SkippedLiquidity != 1 {
		t.Fatalf("skipped liquidity = %d, want 1", result.Stats.SkippedLiquidity)
	}
}
