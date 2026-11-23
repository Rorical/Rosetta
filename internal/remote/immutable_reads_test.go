package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestImmutableReadsSkipRepeatedIdentityReads(t *testing.T) {
	var codes, calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     json.RawMessage
			Method string
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&request)
		result := `"0x00000000000000000000000000000000000000000000000000000000000000aa"`
		switch request.Method {
		case "eth_getCode":
			codes.Add(1)
			result = `"0x6001"`
			if strings.Contains(string(request.Params[0]), "0x00000000000000000000000000000000000000ee") {
				result = `"0x"`
			}
		case "eth_call":
			calls.Add(1)
		}
		w.Write([]byte(`{"jsonrpc":"2.0","id":` + string(request.ID) + `,"result":` + result + `}`))
	}))
	defer server.Close()
	client := rpc.NewClient(server.URL, rpc.Options{})
	chain := domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 99001}
	at := func(n uint64) *snapshot {
		return &snapshot{client: client, chain: chain, keepIdentity: true, block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: n, Hash: domain.Hash{byte(n)}}}}
	}
	pool, empty := domain.Address{0x0a}, domain.Address{19: 0xee}
	for block := uint64(1); block <= 3; block++ {
		s := at(block)
		if hash, err := s.CodeHash(t.Context(), pool); err != nil || hash == (domain.Hash{}) {
			t.Fatal(hash, err)
		}
		if hash, err := s.CodeHash(t.Context(), empty); err != nil || hash != (domain.Hash{}) {
			t.Fatal(hash, err)
		}
		if out, err := s.ReadContract(t.Context(), pool, factorySelector); err != nil || len(out) != 32 || out[31] != 0xaa {
			t.Fatal(out, err)
		}
		if _, err := s.ReadContract(t.Context(), pool, selector("fee()")); err != nil {
			t.Fatal(err)
		}
	}
	// One code and one factory read in total; absent code and fee() every block.
	if codes.Load() != 1+3 || calls.Load() != 1+3 {
		t.Fatalf("eth_getCode %d, eth_call %d", codes.Load(), calls.Load())
	}
	overlaid := at(4)
	overlaid.overlay = map[string]any{}
	if _, err := overlaid.ReadContract(t.Context(), pool, factorySelector); err != nil || calls.Load() != 5 {
		t.Fatal("an overlaid snapshot used the cross-block cache", calls.Load(), err)
	}
}
