package catalog

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// This fixture contains hash-bound Base mainnet RPC responses. Tests never
// contact a network; the block identity makes the observation reproducible.
func TestBaseCatalogAgainstRecordedChainResponses(t *testing.T) {
	raw, err := os.ReadFile("testdata/base-mainnet.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Catalog              json.RawMessage
		ChainID, BlockNumber uint64
		BlockHash            string
		Calls                []struct{ To, Input, Output string }
		CodeHashes           map[string]string
		Pools                []struct {
			Address     string          `json:"address"`
			Kind        domain.PoolKind `json:"kind"`
			Token0      string          `json:"token0"`
			Token1      string          `json:"token1"`
			Fee         uint32          `json:"fee"`
			TickSpacing int32           `json:"tick_spacing"`
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	hash, err := domain.ParseHash(fixture.BlockHash)
	if err != nil || hash == (domain.Hash{}) || fixture.BlockNumber == 0 || fixture.ChainID != 8453 {
		t.Fatal("invalid chain fixture identity")
	}
	reader := &catalogReader{chain: fixture.ChainID, calls: make(map[string][]byte), codes: make(map[domain.Address]domain.Hash)}
	for _, call := range fixture.Calls {
		output, err := hex.DecodeString(strings.TrimPrefix(call.Output, "0x"))
		if err != nil {
			t.Fatal(err)
		}
		reader.calls[call.To+"/"+strings.TrimPrefix(call.Input, "0x")] = output
	}
	for text, code := range fixture.CodeHashes {
		address, err := domain.ParseAddress(text)
		if err != nil {
			t.Fatal(err)
		}
		hash, err := domain.ParseHash(code)
		if err != nil {
			t.Fatal(err)
		}
		reader.codes[address] = hash
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, fixture.Catalog, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	pools, err := Resolve(t.Context(), reader, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != len(fixture.Pools) {
		t.Fatal("catalog differs from recorded chain universe", len(pools), len(fixture.Pools))
	}
	for i, pool := range pools {
		expected := fixture.Pools[i]
		if pool.Address.Hex() != expected.Address || pool.Kind != expected.Kind || pool.Token0.Hex() != expected.Token0 || pool.Token1.Hex() != expected.Token1 || pool.Fee != expected.Fee || pool.TickSpacing != expected.TickSpacing {
			t.Fatalf("pool %d differs from chain fixture: %+v", i, pool)
		}
	}
}
