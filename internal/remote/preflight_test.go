package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
	"github.com/coder/websocket"
)

func preflightStream(t *testing.T, complete bool) *httptest.Server {
	t.Helper()
	metadata := map[string]any{"block_number": 11}
	if complete {
		metadata["receipts"] = map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"payload_id": "0x0102030405060708", "index": 0, "base": map[string]any{"parent_hash": domain.Hash{10}.Hex(), "block_number": "0xb", "timestamp": "0x64"}, "diff": map[string]any{"transactions": []string{}}, "metadata": metadata})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		if err := conn.Write(r.Context(), websocket.MessageText, raw); err != nil {
			return
		}
		conn.Read(r.Context())
	}))
}
func TestStreamPreflightChecksParentAndRejectsStrippedLogs(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(map[bool]string{false: "stripped", true: "complete"}[complete], func(t *testing.T) {
			ws := preflightStream(t, complete)
			defer ws.Close()
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var call struct{ ID uint64 }
				json.NewDecoder(r.Body).Decode(&call)
				result := map[string]any{"number": "0xa", "hash": domain.Hash{10}.Hex(), "parentHash": domain.Hash{9}.Hex(), "timestamp": "0x63", "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
				json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
			}))
			defer server.Close()
			err := checkFlashblocksStream(t.Context(), "ws"+strings.TrimPrefix(ws.URL, "http"), rpc.NewClient(server.URL, rpc.Options{}))
			if complete && (err != nil || calls.Load() != 1) {
				t.Fatal(err, calls.Load())
			}
			if !complete && (err == nil || calls.Load() != 0) {
				t.Fatal("stripped source spent RPC before rejection", err, calls.Load())
			}
		})
	}
}
func TestInvalidStreamFailsBeforePaidPoolBootstrap(t *testing.T) {
	ws := preflightStream(t, false)
	defer ws.Close()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var call struct {
			ID     uint64
			Method string
		}
		json.NewDecoder(r.Body).Decode(&call)
		if call.Method != "eth_chainId" {
			t.Error("bootstrap consumed RPC before source validation", call.Method)
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0xa"})
	}))
	defer server.Close()
	cfg, err := (config.Config{NetworkName: "optimism", RPCURL: server.URL, EventSource: "flashblocks", FlashblocksURL: "ws" + strings.TrimPrefix(ws.URL, "http"), PoolCatalog: "does-not-exist.json"}).ResolveNetwork()
	if err != nil {
		t.Fatal(err)
	}
	if err := run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "omits receipts") {
		t.Fatal("wrong startup failure", err)
	}
	if calls.Load() != 1 {
		t.Fatal("unexpected paid calls", calls.Load())
	}
}

// The stream can name a parent the RPC serves only a moment later, which
// would make startup exit. The parent is waited for briefly.
func TestStreamPreflightWaitsForAParentTheRPCDoesNotServeYet(t *testing.T) {
	ws := preflightStream(t, true)
	defer ws.Close()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&call)
		var result any
		if calls.Add(1) > 2 {
			result = map[string]any{"number": "0xa", "hash": domain.Hash{10}.Hex(), "parentHash": domain.Hash{9}.Hex(), "timestamp": "0x63", "baseFeePerGas": "0x1", "gasLimit": "0x10000", "miner": domain.Address{}.Hex(), "transactions": []string{}}
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": result})
	}))
	defer server.Close()
	if err := checkFlashblocksStream(t.Context(), "ws"+strings.TrimPrefix(ws.URL, "http"), rpc.NewClient(server.URL, rpc.Options{})); err != nil || calls.Load() != 3 {
		t.Fatal(err, calls.Load())
	}
}
