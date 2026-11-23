package remote

import (
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestVelodromeCanonicalFeesReuseMetadataAndBatchCache(t *testing.T) {
	factory, _ := domain.ParseAddress("0xf1046053aa5682b4f9a81b5481394da16be5ff5a")
	p := pool.State{Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindVelodromeV2}, Velodrome: &pool.VelodromeState{Factory: factory, Stable: true, Scale0: big.NewInt(1_000_000), Scale1: big.NewInt(1_000_000_000_000_000_000)}}
	var units atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var calls []struct {
			ID     uint64
			Method string
			Params []json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&calls); err != nil {
			t.Error("expected canonical batch", err)
			w.WriteHeader(500)
			return
		}
		results := []any{}
		for _, call := range calls {
			units.Add(1)
			var args struct{ To, Data string }
			json.Unmarshal(call.Params[0], &args)
			if call.Method != "eth_call" || args.To != factory.Hex() || args.Data != fmt.Sprintf("0xcc56b2c5%064x%064x", p.Key.Address[:], 1) {
				t.Errorf("unexpected read: %s %+v", call.Method, args)
			}
			results = append(results, map[string]any{"id": call.ID, "result": fmt.Sprintf("0x%064x", 5)})
		}
		json.NewEncoder(w).Encode(results)
	}))
	defer server.Close()
	cfg, err := (config.Config{NetworkName: "optimism"}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	client := rpc.NewClient(server.URL, rpc.Options{})
	block := rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{2}}}
	spec := bootstrap.Spec{Address: p.Key.Address, Kind: p.Key.Kind, Velodrome: p.Velodrome}
	fees, cache, err := canonicalFees(t.Context(), cfg, client, block, []bootstrap.Spec{spec})
	if err != nil {
		t.Fatal(err)
	}
	if len(fees) != 1 || fees[0].Fee != 500 {
		t.Fatal(fees)
	}
	reader := &snapshot{client: client, block: block}
	reader.reads.Store(cache)
	_, err = velodromev2.LoadFee(t.Context(), reader, p, "0xa")
	if err != nil {
		t.Fatal(err)
	}
	if units.Load() != 1 {
		t.Fatal("repeated metadata or fee getter", units.Load())
	}
}
