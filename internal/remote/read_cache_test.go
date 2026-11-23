package remote

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestSnapshotReadsReuseOnlyTheSameImmutablePosition(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		var call struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&call)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0x1234"})
	}))
	defer server.Close()
	client := rpc.NewClient(server.URL, rpc.Options{})
	original := &snapshot{client: client, block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}}}
	retained := original.Retain()
	defer retained.Close()
	if _, err := original.ReadContract(t.Context(), domain.Address{1}, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if _, err := retained.ReadContract(t.Context(), domain.Address{1}, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 1 {
		t.Fatalf("duplicate snapshot read used %d RPC calls", count.Load())
	}
	original.Close()
	if _, err := retained.ReadContract(t.Context(), domain.Address{1}, []byte{2}); err != nil {
		t.Fatal("retained cache was invalidated", err)
	}
	other := &snapshot{client: client, block: rpc.ExecutionBlock{Ref: domain.BlockRef{Number: 10, Hash: domain.Hash{99}}}}
	if _, err := other.ReadContract(t.Context(), domain.Address{1}, []byte{2}); err != nil {
		t.Fatal(err)
	}
	if count.Load() != 2 {
		t.Fatal("cache crossed a block hash")
	}
}

func TestConcurrentSnapshotReadsAndErrors(t *testing.T) {
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := count.Add(1)
		var call struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&call)
		if attempt == 1 {
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": -32000, "message": "temporary failure"}})
			return
		}
		time.Sleep(5 * time.Millisecond)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0x1234"})
	}))
	defer server.Close()
	s := &snapshot{client: rpc.NewClient(server.URL, rpc.Options{}), block: rpc.ExecutionBlock{Ref: domain.BlockRef{Hash: domain.Hash{10}}}}
	if _, err := s.ReadContract(t.Context(), domain.Address{1}, []byte{2}); err == nil {
		t.Fatal("fixture error missing")
	}
	var readers sync.WaitGroup
	for i := 0; i < 20; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			if _, err := s.ReadContract(t.Context(), domain.Address{1}, []byte{2}); err != nil {
				t.Error(err)
			}
		}()
	}
	readers.Wait()
	if count.Load() != 2 {
		t.Fatalf("concurrent readers used %d calls, want one failed and one coalesced successful request", count.Load())
	}
	for _, value := range []string{"0x1", "0x2", "0x1"} {
		if _, err := s.contractCall(t.Context(), domain.Address{1}, []byte{2}, map[string]any{domain.Address{3}.Hex(): map[string]any{"balance": value}}); err != nil {
			t.Fatal(err)
		}
	}
	if count.Load() != 4 {
		t.Fatal("state overrides shared the wrong cached result", count.Load())
	}
}
