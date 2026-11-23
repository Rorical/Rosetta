package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestSnapshotBatchesAndReusesIdenticalContractReads(t *testing.T) {
	var methods, requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		if raw[0] != '[' {
			t.Error("contract batch was sent as individual calls")
			w.WriteHeader(500)
			return
		}
		var calls []struct{ ID uint64 }
		json.Unmarshal(raw, &calls)
		var rows []any
		for _, call := range calls {
			methods.Add(1)
			rows = append(rows, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0x1234"})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	s := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}}}
	a := pool.ContractCall{To: domain.Address{1}, Data: []byte{1}, Block: "0xa"}
	b := a
	b.Data = []byte{2}
	values, err := s.CallContracts(t.Context(), []pool.ContractCall{a, b, a})
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err = s.CallContract(t.Context(), a.To, a.Data, a.Block, &value); err != nil {
		t.Fatal(err)
	}
	if len(values) != 3 || value != "0x1234" || methods.Load() != 2 || requests.Load() != 1 {
		t.Fatal(values, value, methods.Load(), requests.Load())
	}
}

func TestSnapshotContractChunksRunConcurrentlyWithinBound(t *testing.T) {
	var inFlight, peak atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := inFlight.Add(1)
		defer inFlight.Add(-1)
		for old := peak.Load(); now > old && !peak.CompareAndSwap(old, now); old = peak.Load() {
		}
		time.Sleep(20 * time.Millisecond)
		var calls []struct {
			ID     uint64
			Params []json.RawMessage
		}
		json.NewDecoder(r.Body).Decode(&calls)
		var rows []any
		for _, call := range calls {
			var request struct{ Data string }
			json.Unmarshal(call.Params[0], &request)
			if request.Data == "0xff" {
				rows = append(rows, map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32000, "message": "boom"}})
				continue
			}
			rows = append(rows, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0x01"})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	s := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}}}
	var calls []pool.ContractCall
	for i := range 20 * batchChunk {
		calls = append(calls, pool.ContractCall{To: domain.Address{byte(i)}, Data: []byte{byte(i % 250)}, Block: "0xa"})
	}
	values, err := s.CallContracts(t.Context(), calls)
	if err != nil || len(values) != len(calls) {
		t.Fatalf("values %d err %v", len(values), err)
	}
	if got := peak.Load(); got < 2 || got > batchInFlight {
		t.Fatalf("peak concurrent batches = %d, want 2..%d", got, batchInFlight)
	}

	failing := append([]pool.ContractCall{}, calls[:batchChunk]...)
	failing = append(failing, pool.ContractCall{To: domain.Address{0xee}, Data: []byte{0xff}, Block: "0xa"})
	if _, err := (&snapshot{client: s.client, block: s.block}).CallContracts(t.Context(), failing); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("member failure not reported: %v", err)
	}
}
