package catalog

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
)

// metricImmutables answers getImmutables() from a pool fixture's recorded
// immutables, at the word positions the fixture script decoded them from.
func metricImmutables(t *testing.T, pool string) []byte {
	t.Helper()
	raw, err := os.ReadFile("../dex/metricomm/testdata/" + pool + ".json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Immutables struct {
			Token0     string   `json:"token0"`
			Token1     string   `json:"token1"`
			Scale0     string   `json:"token0_scale_multiplier"`
			Scale1     string   `json:"token1_scale_multiplier"`
			Lowest     int64    `json:"lowest_bin"`
			Highest    int64    `json:"highest_bin"`
			Extensions []string `json:"extensions"`
		} `json:"immutables"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	output := make([]byte, 24*32)
	put := func(i int, value *big.Int) {
		if value.Sign() < 0 {
			value = new(big.Int).Add(value, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		value.FillBytes(output[i*32 : (i+1)*32])
	}
	address := func(i int, text string) {
		parsed, err := domain.ParseAddress(text)
		if err != nil {
			t.Fatal(err)
		}
		copy(output[i*32+12:], parsed[:])
	}
	im := fixture.Immutables
	address(1, im.Token0)
	address(2, im.Token1)
	for i, text := range []string{im.Scale0, im.Scale1} {
		value, _ := new(big.Int).SetString(text, 10)
		put(3+i, value)
	}
	put(9, big.NewInt(im.Lowest))
	put(10, big.NewInt(im.Highest))
	for i, extension := range im.Extensions {
		address(11+i, extension)
	}
	return output
}

// The Arbitrum One catalog names both MetricOmm pools. Resolving them reads
// each pool's immutables (the fixtures' own, block 523362412) and yields a
// spec with the pool's tokens and registration, and nothing read from storage.
func TestArbitrumCatalogResolvesMetricOmmPools(t *testing.T) {
	cfg, err := Load("../../configs/arbitrum-one-catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.MetricOmm) != 2 {
		t.Fatalf("%d MetricOmm pools in the catalog", len(cfg.MetricOmm))
	}
	// Only the MetricOmm part is resolved here; the native token needs V4.
	cfg.Factories, cfg.V4 = nil, nil
	cfg.Tokens = slices.DeleteFunc(cfg.Tokens, func(token domain.Address) bool { return token == (domain.Address{}) })
	reader := &catalogReader{chain: 42161, calls: make(map[string][]byte), codes: make(map[domain.Address]domain.Hash)}
	for _, token := range cfg.Tokens {
		reader.codes[token] = domain.Hash{1}
	}
	for _, p := range cfg.MetricOmm {
		for _, contract := range []domain.Address{p.Pool, p.Oracle, p.QuoteExtension, p.Reference} {
			reader.codes[contract] = domain.Hash{1}
		}
		reader.calls[p.Pool.Hex()+fmt.Sprintf("/%x", metricomm.ImmutablesCall())] = metricImmutables(t, p.Pool.Hex())
	}
	specs, err := Resolve(t.Context(), reader, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("%d specs", len(specs))
	}
	weth, wbtc, usdc := "0x82af49447d8a07e3bd95bd0d56f35241523fbab1", "0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f", "0xaf88d065e77c8cc2239327c5edb3a432268e5831"
	want := []struct {
		token0  string
		feed    uint8
		lowest  int16
		highest int16
		scale0  int64
		// reference is the pool's base feed on the reference oracle.
		reference uint32
	}{{weth, 1, -20, 19, 1, 2}, {wbtc, 2, -30, 29, 10_000_000_000, 1}}
	for i, spec := range specs {
		m := spec.Metric
		if spec.Kind != domain.PoolKindMetricOmm || spec.Address != cfg.MetricOmm[i].Pool || spec.Token0.Hex() != want[i].token0 || spec.Token1.Hex() != usdc || spec.Fee != 0 {
			t.Fatalf("spec %d: %+v", i, spec)
		}
		if m == nil || !m.Stale || m.Words != nil || m.FeedIndex != want[i].feed || m.LowestBin != want[i].lowest || m.HighestBin != want[i].highest ||
			m.Token0Scale.Int64() != want[i].scale0 || m.Token1Scale.Int64() != 1_000_000_000_000 || m.Oracle != cfg.MetricOmm[i].Oracle || m.QuoteExtensionSlot != cfg.MetricOmm[i].QuoteExtensionSlot ||
			m.Reference.Hex() != "0x30a88bf894d1b806125a23ebf23d715496fd7102" || m.ReferenceBaseFeed != want[i].reference || m.ReferenceQuoteFeed != 7 {
			t.Fatalf("registration %d: %+v", i, m)
		}
	}

	// A pool whose token is outside the catalog's universe is refused.
	narrow := cfg
	narrow.Tokens = nil
	for _, token := range cfg.Tokens {
		if token.Hex() != wbtc {
			narrow.Tokens = append(narrow.Tokens, token)
		}
	}
	if _, err := Resolve(t.Context(), reader, narrow); err == nil {
		t.Fatal("a pool trading an unlisted token was registered")
	}
	// So is an entry whose feed is not one of the oracle's.
	invalid := cfg
	invalid.MetricOmm = append([]MetricPool(nil), cfg.MetricOmm...)
	invalid.MetricOmm[0].FeedIndex = metricomm.OracleFeedCount
	if _, err := Resolve(t.Context(), reader, invalid); err == nil {
		t.Fatal("an unknown feed index was accepted")
	}
}
