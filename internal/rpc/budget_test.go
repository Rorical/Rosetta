package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func TestSharedBudgetCountsBatchMembers(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var batch []struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&batch)
		out := make([]any, len(batch))
		for i, call := range batch {
			out[i] = map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0xa"}
		}
		json.NewEncoder(w).Encode(out)
	}))
	defer server.Close()
	var units atomic.Uint64
	var operationUnits atomic.Uint64
	policy := NewRequestPolicy(Options{MaxCalls: 3, Observe: func(o RequestObservation) { units.Add(o.Calls) }})
	ctx := WithRequestPolicy(t.Context(), policy)
	ctx = WithRequestObserver(ctx, func(o RequestObservation) { operationUnits.Add(o.Calls) })
	client := NewClient(server.URL, Options{})
	if _, err := client.Batch(ctx, []Call{{Method: "eth_chainId"}, {Method: "eth_chainId"}}); err != nil {
		t.Fatal(err)
	}
	// A separate RPC client shares the same policy. It cannot bypass the limit,
	// and rejected requests never reach the provider.
	other := NewClient(server.URL, Options{})
	if _, err := other.Batch(ctx, []Call{{Method: "eth_chainId"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := other.Batch(ctx, []Call{{Method: "eth_chainId"}}); !errors.Is(err, ErrCallBudgetExceeded) {
		t.Fatal(err)
	}
	if requests.Load() != 2 || units.Load() != 3 {
		t.Fatal("budget leaked", requests.Load(), units.Load())
	}
	if operationUnits.Load() != 3 {
		t.Fatal("operation observer lost to shared policy", operationUnits.Load())
	}
}
func TestRetryConsumesSharedBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusTooManyRequests) }))
	defer server.Close()
	client := NewClient(server.URL, Options{MaxCalls: 1, MaxRetries: 2})
	if err := client.Call(context.Background(), "eth_chainId", nil, nil); !errors.Is(err, ErrCallBudgetExceeded) {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("retry bypassed call budget")
	}
}

func TestConcurrentClientsCannotOverspendSharedBudget(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&request)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": "0xa"})
	}))
	defer server.Close()
	ctx := WithRequestPolicy(t.Context(), NewRequestPolicy(Options{MaxCalls: 7}))
	var workers sync.WaitGroup
	for i := 0; i < 30; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			err := NewClient(server.URL, Options{}).Call(ctx, "eth_chainId", nil, nil)
			if err != nil && !errors.Is(err, ErrCallBudgetExceeded) {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	if calls.Load() != 7 {
		t.Fatal("concurrent budget accounting failed", calls.Load())
	}
}
