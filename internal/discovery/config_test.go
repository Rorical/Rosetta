package discovery

import (
	"errors"
	"fmt"
	"math/big"
	"reflect"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestLoadConfigDefaults(t *testing.T) {
	quote := repeatedAddress(0xc0)
	config, err := LoadConfig([]byte(fmt.Sprintf(`{"quote_tokens":[%q]}`, quote.Hex())))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.LookbackBlocks != 50_000 || config.RefreshBlocks != 300 || config.ScanChunkBlocks != 10_000 {
		t.Fatalf("block settings = %d/%d/%d, want 50000/300/10000",
			config.LookbackBlocks, config.RefreshBlocks, config.ScanChunkBlocks)
	}
	if config.MaxPools != 32 || config.MaxFee != 10_000 {
		t.Fatalf("max pools/fee = %d/%d, want 32/10000", config.MaxPools, config.MaxFee)
	}
	if config.MinLiquidity.Sign() != 0 {
		t.Fatalf("min liquidity = %s, want 0", config.MinLiquidity)
	}
	if !reflect.DeepEqual(config.Sources, allSources()) {
		t.Fatalf("sources = %v, want %v", config.Sources, allSources())
	}
	if len(config.Seeds) != 0 {
		t.Fatalf("seeds = %v, want none", config.Seeds)
	}
	if len(config.QuoteTokens) != 1 || config.QuoteTokens[0] != quote {
		t.Fatalf("quote tokens = %v, want [%s]", config.QuoteTokens, quote)
	}
}

func TestLoadConfigFull(t *testing.T) {
	quote := repeatedAddress(0xc0)
	other := repeatedAddress(0xc1)
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	v3Pool := repeatedAddress(0x33)

	v4ID, err := uniswapv4.PoolID(token0, token1, 500, 10, domain.Address{})
	if err != nil {
		t.Fatalf("PoolID: %v", err)
	}

	document := fmt.Sprintf(`{
		"lookback_blocks": 1000,
		"refresh_blocks": 60,
		"scan_chunk_blocks": 500,
		"max_pools": 4,
		"min_liquidity": "1000000",
		"max_fee": 500,
		"quote_tokens": [%q, %q],
		"sources": ["uniswap_v3", "uniswap_v4"],
		"seed_pools": [
			{"address": %q, "token0": %q, "token1": %q, "kind": "v3", "fee": 500, "tick_spacing": 10},
			{"address": %q, "token0": %q, "token1": %q, "kind": "v4", "pool_id": %q, "fee": 500, "tick_spacing": 10}
		]
	}`, quote.Hex(), other.Hex(), v3Pool.Hex(), quote.Hex(), other.Hex(),
		v4PoolManager.Hex(), token0.Hex(), token1.Hex(), v4ID.Hex())

	config, err := LoadConfig([]byte(document))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.LookbackBlocks != 1000 || config.RefreshBlocks != 60 || config.ScanChunkBlocks != 500 {
		t.Fatalf("block settings = %d/%d/%d, want 1000/60/500",
			config.LookbackBlocks, config.RefreshBlocks, config.ScanChunkBlocks)
	}
	if config.MaxPools != 4 || config.MaxFee != 500 {
		t.Fatalf("max pools/fee = %d/%d, want 4/500", config.MaxPools, config.MaxFee)
	}
	if config.MinLiquidity.Cmp(big.NewInt(1_000_000)) != 0 {
		t.Fatalf("min liquidity = %s, want 1000000", config.MinLiquidity)
	}
	if !reflect.DeepEqual(config.Sources, []Source{SourceUniswapV3, SourceUniswapV4}) {
		t.Fatalf("sources = %v, want uniswap_v3, uniswap_v4", config.Sources)
	}
	if len(config.QuoteTokens) != 2 || config.QuoteTokens[0] != quote || config.QuoteTokens[1] != other {
		t.Fatalf("quote tokens = %v", config.QuoteTokens)
	}
	if len(config.Seeds) != 2 {
		t.Fatalf("len(seeds) = %d, want 2", len(config.Seeds))
	}
	if config.Seeds[0].Address != v3Pool || config.Seeds[0].Kind != domain.PoolKindV3 {
		t.Fatalf("v3 seed = %+v", config.Seeds[0])
	}
	if config.Seeds[1].PoolID != v4ID || config.Seeds[1].Kind != domain.PoolKindV4 {
		t.Fatalf("v4 seed = %+v", config.Seeds[1])
	}
}

func TestLoadConfigErrors(t *testing.T) {
	quote := repeatedAddress(0xc0)
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	v3Pool := repeatedAddress(0x33)
	quotes := fmt.Sprintf(`"quote_tokens":[%q]`, quote.Hex())
	someHash := mustHash(t, "0x"+strings.Repeat("11", 32)).Hex()

	v3Fields := func(extra string) string {
		return fmt.Sprintf(`"address":%q,"token0":%q,"token1":%q,"kind":"v3","fee":500,"tick_spacing":10%s`,
			v3Pool.Hex(), token0.Hex(), token1.Hex(), extra)
	}
	seed := func(fields string) string {
		return fmt.Sprintf(`{%s,"seed_pools":[{ %s }]}`, quotes, fields)
	}
	fields := func(address domain.Address, kind, extra string) string {
		return fmt.Sprintf(`"address":%q,"token0":%q,"token1":%q,"kind":%q%s`,
			address.Hex(), token0.Hex(), token1.Hex(), kind, extra)
	}

	tests := []struct {
		name     string
		document string
	}{
		{"invalid json", `{`},
		{"unknown field", fmt.Sprintf(`{%s,"extra":1}`, quotes)},
		{"trailing data", fmt.Sprintf(`{%s}{}`, quotes)},
		{"empty sources", fmt.Sprintf(`{%s,"sources":[]}`, quotes)},
		{"unknown source", fmt.Sprintf(`{%s,"sources":["curve"]}`, quotes)},
		{"duplicate source", fmt.Sprintf(`{%s,"sources":["uniswap_v3","uniswap_v3"]}`, quotes)},
		{"zero scan chunk", fmt.Sprintf(`{%s,"scan_chunk_blocks":0}`, quotes)},
		{"zero max pools", fmt.Sprintf(`{%s,"max_pools":0}`, quotes)},
		{"negative max pools", fmt.Sprintf(`{%s,"max_pools":-1}`, quotes)},
		{"max fee too high", fmt.Sprintf(`{%s,"max_fee":1000001}`, quotes)},
		{"negative min liquidity", fmt.Sprintf(`{%s,"min_liquidity":"-1"}`, quotes)},
		{"bad min liquidity", fmt.Sprintf(`{%s,"min_liquidity":"12x"}`, quotes)},
		{"bad quote token", `"quote_tokens":["0x12"]`},
		{"unknown seed kind", seed(fields(v3Pool, "camelot_v2", `,"fee":500,"tick_spacing":10`))},
		{"v2 missing fee", seed(fields(v3Pool, "v2", ""))},
		{"zero seed address", seed(fields(domain.Address{}, "v3", `,"fee":500,"tick_spacing":10`))},
		{"v3 seed with pool id", seed(v3Fields(fmt.Sprintf(`,"pool_id":%q`, someHash)))},
		{"v3 seed with hooks", seed(v3Fields(fmt.Sprintf(`,"hooks":%q`, v3Pool.Hex())))},
		{"v3 seed zero fee", seed(fmt.Sprintf(`"address":%q,"token0":%q,"token1":%q,"kind":"v3","fee":0,"tick_spacing":10`,
			v3Pool.Hex(), token0.Hex(), token1.Hex()))},
		{"v3 seed zero spacing", seed(fmt.Sprintf(`"address":%q,"token0":%q,"token1":%q,"kind":"v3","fee":500,"tick_spacing":0`,
			v3Pool.Hex(), token0.Hex(), token1.Hex()))},
		{"v4 seed zero id", seed(fields(v4PoolManager, "v4", `,"fee":500,"tick_spacing":10`))},
		{"v4 seed wrong manager", seed(fields(v3Pool, "v4", fmt.Sprintf(`,"pool_id":%q,"fee":500,"tick_spacing":10`, someHash)))},
		{"v4 seed id mismatch", seed(fields(v4PoolManager, "v4", fmt.Sprintf(`,"pool_id":%q,"fee":500,"tick_spacing":10`, someHash)))},
		{"v4 seed with hooks", seed(fields(v4PoolManager, "v4", fmt.Sprintf(`,"pool_id":%q,"hooks":%q,"fee":500,"tick_spacing":10`, someHash, v3Pool.Hex())))},
		{"duplicate seeds", fmt.Sprintf(`{%s,"seed_pools":[{ %s },{ %s }]}`, quotes, v3Fields(""), v3Fields(""))},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := LoadConfig([]byte(test.document)); err == nil {
				t.Fatal("LoadConfig succeeded, want error")
			}
		})
	}
}

func TestParseSource(t *testing.T) {
	tests := []struct {
		text string
		want Source
	}{
		{"uniswap_v3", SourceUniswapV3},
		{"Pancake_V3", SourcePancakeV3},
		{" slipstream ", SourceSlipstream},
		{"uniswap_v4", SourceUniswapV4},
	}
	for _, test := range tests {
		source, err := ParseSource(test.text)
		if err != nil {
			t.Fatalf("ParseSource(%q): %v", test.text, err)
		}
		if source != test.want {
			t.Fatalf("ParseSource(%q) = %v, want %v", test.text, source, test.want)
		}
	}
	if _, err := ParseSource("curve"); err == nil {
		t.Fatal("ParseSource(curve) succeeded, want error")
	}
}

func TestLoadConfigAllowsZeroLookback(t *testing.T) {
	quote := repeatedAddress(0xc0)
	document := fmt.Sprintf(`{"quote_tokens":[%q],"lookback_blocks":0}`, quote.Hex())
	config, err := LoadConfig([]byte(document))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if config.LookbackBlocks != 0 {
		t.Fatalf("lookback = %d, want 0", config.LookbackBlocks)
	}
}

func TestLoadConfigRejectsV4SeedWithHooks(t *testing.T) {
	token0 := repeatedAddress(0x01)
	token1 := repeatedAddress(0x77)
	hooks := repeatedAddress(0x99)
	id, err := uniswapv4.PoolID(token0, token1, 500, 10, hooks)
	if err != nil {
		t.Fatalf("PoolID: %v", err)
	}

	document := fmt.Sprintf(`{
		"quote_tokens": [%q],
		"seed_pools": [{
			"address": %q, "token0": %q, "token1": %q, "kind": "v4",
			"pool_id": %q, "hooks": %q, "fee": 500, "tick_spacing": 10
		}]
	}`, token0.Hex(), v4PoolManager.Hex(), token0.Hex(), token1.Hex(), id.Hex(), hooks.Hex())

	_, err = LoadConfig([]byte(document))
	if !errors.Is(err, ErrInvalidConfig) {
		t.Fatalf("error = %v, want ErrInvalidConfig", err)
	}
	if !strings.Contains(err.Error(), "hooks") {
		t.Fatalf("error = %v, want a hooks explanation", err)
	}
}

func TestLoadConfigAllowsGlobalQuoteTokens(t *testing.T) {
	documents := []string{
		`{}`,                  // absent: global discovery
		`{"quote_tokens":[]}`, // explicit empty: same
	}
	for _, document := range documents {
		config, err := LoadConfig([]byte(document))
		if err != nil {
			t.Fatalf("LoadConfig(%s): %v", document, err)
		}
		if len(config.QuoteTokens) != 0 {
			t.Fatalf("LoadConfig(%s) quote tokens = %v, want none", document, config.QuoteTokens)
		}
	}
}
