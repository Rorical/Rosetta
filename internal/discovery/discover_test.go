package discovery

import (
	"context"
	"math/big"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestDiscoverRanksByLiquidity(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	poolA := repeatedAddress(0x31)
	poolB := repeatedAddress(0x32)
	poolC := repeatedAddress(0x33)

	logs := newFakeLogs()
	logs.add(
		v3Log(quote0, quote1, 500, 10, poolA),
		v3Log(quote0, quote1, 500, 10, poolB),
		v3Log(quote0, quote1, 500, 10, poolC),
	)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: poolA, Kind: domain.PoolKindV3}, big.NewInt(100))
	caller.setLiquidity(Spec{Address: poolB, Kind: domain.PoolKindV3}, big.NewInt(300))
	caller.setLiquidity(Spec{Address: poolC, Kind: domain.PoolKindV3}, big.NewInt(200))

	config := testConfig([]Source{SourceUniswapV3}, quote0, quote1)
	config.MaxPools = 2
	discoverer, err := New(logs, caller, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result, err := discoverer.Discover(context.Background(), 100, 200)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 2 {
		t.Fatalf("len(pools) = %d, want 2", len(result.Pools))
	}
	if result.Pools[0].Address != poolB || result.Pools[1].Address != poolC {
		t.Fatalf("pools = [%s %s], want [%s %s]",
			result.Pools[0].Address, result.Pools[1].Address, poolB, poolC)
	}
	if result.Stats.Selected != 2 || result.Stats.Decoded != 3 || result.Stats.Logs != 3 {
		t.Fatalf("stats = %+v, want selected 2, decoded 3, logs 3", result.Stats)
	}
	for _, call := range caller.calls {
		if call.Block != "0xc8" {
			t.Fatalf("call block = %q, want 0xc8", call.Block)
		}
	}
}

func TestDiscoverTieBreakByPoolKey(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	first := repeatedAddress(0x10)
	second := repeatedAddress(0x20)

	logs := newFakeLogs()
	// Deposit the higher address first so only the sort can order them.
	logs.add(
		v3Log(quote0, quote1, 500, 10, second),
		v3Log(quote0, quote1, 500, 10, first),
	)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: first, Kind: domain.PoolKindV3}, big.NewInt(7))
	caller.setLiquidity(Spec{Address: second, Kind: domain.PoolKindV3}, big.NewInt(7))

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV3}, quote0, quote1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 2 || result.Pools[0].Address != first || result.Pools[1].Address != second {
		t.Fatalf("pools = %v, want ascending address order", result.Pools)
	}
}

func TestDiscoverV4SiblingsStayDistinct(t *testing.T) {
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)

	first := v4Log(t, token0, token1, 500, 10, domain.Address{})
	second := v4Log(t, token0, token1, 3000, 60, domain.Address{})
	if first.Topics[1] == second.Topics[1] {
		t.Fatal("fixture produced identical pool IDs")
	}

	logs := newFakeLogs()
	logs.add(first, second)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: v4PoolManager, Kind: domain.PoolKindV4, PoolID: first.Topics[1]}, big.NewInt(10))
	caller.setLiquidity(Spec{Address: v4PoolManager, Kind: domain.PoolKindV4, PoolID: second.Topics[1]}, big.NewInt(20))

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV4}, token0, token1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 2 {
		t.Fatalf("len(pools) = %d, want 2", len(result.Pools))
	}
	if result.Pools[0].PoolID == result.Pools[1].PoolID {
		t.Fatal("pools share a pool ID")
	}
	if result.Pools[0].PoolID != second.Topics[1] {
		t.Fatalf("first pool = %s, want the higher-liquidity %s",
			result.Pools[0].PoolID.Hex(), second.Topics[1].Hex())
	}
}

func TestDiscoverDeduplicatesLogs(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	poolAddress := repeatedAddress(0x41)

	duplicate := v3Log(quote0, quote1, 500, 10, poolAddress)
	logs := newFakeLogs()
	logs.add(duplicate, duplicate)

	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: poolAddress, Kind: domain.PoolKindV3}, big.NewInt(1))

	discoverer, err := New(logs, caller, testConfig([]Source{SourceUniswapV3}, quote0, quote1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(result.Pools))
	}
	if result.Stats.Logs != 2 || result.Stats.Decoded != 2 || result.Stats.Selected != 1 {
		t.Fatalf("stats = %+v, want logs 2, decoded 2, selected 1", result.Stats)
	}
}

func TestDiscoverSeeds(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	discovered := repeatedAddress(0x51)
	seedHigh := repeatedAddress(0x92)
	seedLow := repeatedAddress(0x91)

	logs := newFakeLogs()
	logs.add(v3Log(quote0, quote1, 500, 10, discovered))
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: discovered, Kind: domain.PoolKindV3}, big.NewInt(500))

	seed := func(address domain.Address) Spec {
		return Spec{
			Address:     address,
			Token0:      quote0,
			Token1:      quote1,
			Kind:        domain.PoolKindV3,
			Fee:         500,
			TickSpacing: 10,
		}
	}

	config := testConfig([]Source{SourceUniswapV3}, quote0, quote1)
	config.MaxPools = 1
	// The first seed repeats the discovered pool's key; it must not appear
	// twice, and it must not push a discovered pool out of the cap.
	config.Seeds = []Spec{seed(discovered), seed(seedHigh), seed(seedLow)}

	discoverer, err := New(logs, caller, config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(result.Pools) != 3 {
		t.Fatalf("len(pools) = %d, want 3", len(result.Pools))
	}
	if result.Pools[0].Address != discovered {
		t.Fatalf("pools[0] = %s, want discovered %s", result.Pools[0].Address, discovered)
	}
	if result.Pools[1].Address != seedLow || result.Pools[2].Address != seedHigh {
		t.Fatalf("seed order = [%s %s], want [%s %s]",
			result.Pools[1].Address, result.Pools[2].Address, seedLow, seedHigh)
	}
	if len(result.Seeds) != 3 {
		t.Fatalf("len(seeds) = %d, want 3", len(result.Seeds))
	}
	if result.Stats.Selected != 1 {
		t.Fatalf("selected = %d, want 1", result.Stats.Selected)
	}
}

func TestDiscoverChunksAndFilters(t *testing.T) {
	quote := repeatedAddress(0xc0)
	logs := newFakeLogs()
	config := testConfig([]Source{SourceUniswapV3, SourceUniswapV4}, quote)
	config.ScanChunkBlocks = 10_000

	discoverer, err := New(logs, newFakeCaller(), config)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 100, 25_000)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := uint64(24_901 * 2); result.Stats.ScannedBlocks != want {
		t.Fatalf("scanned blocks = %d, want %d", result.Stats.ScannedBlocks, want)
	}

	if len(logs.filters) != 6 {
		t.Fatalf("len(filters) = %d, want 6", len(logs.filters))
	}
	wantRanges := [][2]uint64{{100, 10_099}, {10_100, 20_099}, {20_100, 25_000}}
	for i, want := range wantRanges {
		got := logs.filters[i]
		if got.FromBlock != want[0] || got.ToBlock != want[1] {
			t.Fatalf("filter %d range = %d-%d, want %d-%d", i, got.FromBlock, got.ToBlock, want[0], want[1])
		}
		if len(got.Addresses) != 1 || got.Addresses[0] != uniswapV3Factory {
			t.Fatalf("filter %d address = %v, want %s", i, got.Addresses, uniswapV3Factory)
		}
		if len(got.Topics) != 1 || len(got.Topics[0]) != 1 || got.Topics[0][0] != topicV3PoolCreated {
			t.Fatalf("filter %d topics = %v, want the v3 signature", i, got.Topics)
		}
	}
	for i := 3; i < 6; i++ {
		got := logs.filters[i]
		if got.FromBlock != wantRanges[i-3][0] || got.ToBlock != wantRanges[i-3][1] {
			t.Fatalf("v4 filter %d range = %d-%d", i, got.FromBlock, got.ToBlock)
		}
		if len(got.Addresses) != 1 || got.Addresses[0] != v4PoolManager {
			t.Fatalf("v4 filter %d address = %v, want %s", i, got.Addresses, v4PoolManager)
		}
		if len(got.Topics) != 2 || len(got.Topics[0]) != 1 || got.Topics[0][0] != topicV4Initialize || got.Topics[1] != nil {
			t.Fatalf("v4 filter %d topics = %v, want signature plus a nil ID position", i, got.Topics)
		}
	}
}

func TestDiscoverDeterministic(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	poolA := repeatedAddress(0x61)
	poolB := repeatedAddress(0x62)

	build := func() (*Discoverer, error) {
		logs := newFakeLogs()
		logs.add(
			v3Log(quote0, quote1, 500, 10, poolA),
			v3Log(quote0, quote1, 3000, 60, poolB),
		)
		caller := newFakeCaller()
		caller.setLiquidity(Spec{Address: poolA, Kind: domain.PoolKindV3}, big.NewInt(11))
		caller.setLiquidity(Spec{Address: poolB, Kind: domain.PoolKindV3}, big.NewInt(11))
		config := testConfig([]Source{SourceUniswapV3}, quote0, quote1)
		config.Seeds = []Spec{{Address: repeatedAddress(0x93), Token0: quote0, Token1: quote1, Kind: domain.PoolKindV3, Fee: 500, TickSpacing: 10}}
		return New(logs, caller, config)
	}

	firstDiscoverer, err := build()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	secondDiscoverer, err := build()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	first, err := firstDiscoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	second, err := secondDiscoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("results differ:\n%+v\n%+v", first, second)
	}
}

func TestDiscoverPancakeV3Source(t *testing.T) {
	quote0 := repeatedAddress(0xc0)
	quote1 := repeatedAddress(0xc1)
	poolAddress := repeatedAddress(0x71)

	log := v3Log(quote0, quote1, 100, 1, poolAddress)
	log.Address = pancakeV3Factory
	logs := newFakeLogs()
	logs.add(log)
	caller := newFakeCaller()
	caller.setLiquidity(Spec{Address: poolAddress, Kind: domain.PoolKindV3}, big.NewInt(1))

	discoverer, err := New(logs, caller, testConfig([]Source{SourcePancakeV3}, quote0, quote1))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := discoverer.Discover(context.Background(), 1, 2)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(logs.filters) != 1 || logs.filters[0].Addresses[0] != pancakeV3Factory {
		t.Fatalf("filters = %v, want the Pancake factory", logs.filters)
	}
	if len(result.Pools) != 1 {
		t.Fatalf("len(pools) = %d, want 1", len(result.Pools))
	}
	if result.Pools[0].Address != poolAddress || result.Pools[0].Kind != domain.PoolKindV3 {
		t.Fatalf("pool = %+v, want the pancake pool under the v3 engine", result.Pools[0])
	}
}

func TestDiscoveryUsesOnlySelectedNetworkDeployments(t *testing.T) {
	custom := domain.Address{0x99}
	deployments := &domain.Deployments{Factories: map[string]domain.Address{"uniswap_v3": custom}}
	cfg, err := LoadConfigForNetwork([]byte(`{}`), deployments)
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 1 || cfg.Sources[0] != SourceUniswapV3 {
		t.Fatal("default sources include another chain's deployments")
	}
	if _, err := LoadConfigForNetwork([]byte(`{"sources":["uniswap_v4"]}`), deployments); err == nil {
		t.Fatal("missing deployment accepted")
	}
	log := v3Log(repeatedAddress(1), repeatedAddress(2), 500, 10, repeatedAddress(3))
	if _, matched, _ := DecodeCreation(SourceUniswapV3, log, deployments); matched {
		t.Fatal("Arbitrum One factory accepted")
	}
	log.Address = custom
	spec, matched, err := DecodeCreation(SourceUniswapV3, log, deployments)
	if err != nil || !matched || spec.Address != repeatedAddress(3) {
		t.Fatal("selected factory not decoded", err)
	}
}
