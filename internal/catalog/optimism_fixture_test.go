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

// Fixed-hash OP Mainnet responses include real V2 pairs, native ETH and three
// distinct hookless V4 IDs behind one manager. Unit tests never access RPC.
func TestOptimismV2V4RecordedCatalog(t *testing.T) {
	raw, err := os.ReadFile("testdata/optimism-v2-v4.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		ChainID, BlockNumber uint64
		BlockHash            string
		Catalog              json.RawMessage
		Calls                []struct{ To, Input, Output string }
		CodeHashes           map[string]string
		Pools                []struct {
			Address     string          `json:"address"`
			Kind        domain.PoolKind `json:"kind"`
			PoolID      string          `json:"pool_id"`
			Token0      string          `json:"token0"`
			Token1      string          `json:"token1"`
			Fee         uint32          `json:"fee"`
			TickSpacing int32           `json:"tick_spacing"`
		}
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.ChainID != 10 || fixture.BlockNumber != 158939988 {
		t.Fatal("fixture identity changed")
	}
	if _, err := domain.ParseHash(fixture.BlockHash); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "catalog.json")
	if err := os.WriteFile(path, fixture.Catalog, 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	base := &catalogReader{chain: 10, calls: map[string][]byte{}, codes: map[domain.Address]domain.Hash{}}
	for _, call := range fixture.Calls {
		value, err := hex.DecodeString(strings.TrimPrefix(call.Output, "0x"))
		if err != nil {
			t.Fatal(err)
		}
		base.calls[call.To+"/"+strings.TrimPrefix(call.Input, "0x")] = value
	}
	for text, hash := range fixture.CodeHashes {
		address, err := domain.ParseAddress(text)
		if err != nil {
			t.Fatal(err)
		}
		code, err := domain.ParseHash(hash)
		if err != nil {
			t.Fatal(err)
		}
		base.codes[address] = code
	}
	reader := &v4CatalogReader{catalogReader: base, manager: cfg.V4.Manager, view: cfg.V4.StateView}
	pools, err := Resolve(t.Context(), reader, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(pools) != 6 || len(pools) != len(fixture.Pools) {
		t.Fatal("recorded pool coverage lost", len(pools))
	}
	for i, pool := range pools {
		want := fixture.Pools[i]
		if pool.Address.Hex() != want.Address || pool.PoolID.Hex() != want.PoolID || pool.Kind != want.Kind || pool.Token0.Hex() != want.Token0 || pool.Token1.Hex() != want.Token1 || pool.Fee != want.Fee || pool.TickSpacing != want.TickSpacing {
			t.Fatalf("pool %d differs from chain fixture: %+v", i, pool)
		}
	}
}
