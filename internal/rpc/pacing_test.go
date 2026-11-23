package rpc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestRequestPacingIncludesConcurrentCallsAndRetries(t *testing.T) {
	var mu sync.Mutex
	var arrivals []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		arrivals = append(arrivals, time.Now())
		first := len(arrivals) == 1
		mu.Unlock()
		if first {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		var request struct {
			ID uint64 `json:"id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": "0xa"})
	}))
	defer server.Close()
	client := NewClient(server.URL, Options{MinInterval: 30 * time.Millisecond, MaxRetries: 1})
	var calls sync.WaitGroup
	for i := 0; i < 3; i++ {
		calls.Add(1)
		go func() {
			defer calls.Done()
			var result string
			if err := client.Call(t.Context(), "eth_chainId", []any{}, &result); err != nil {
				t.Error(err)
			}
		}()
	}
	calls.Wait()
	if len(arrivals) != 4 {
		t.Fatalf("requests = %d, want 4", len(arrivals))
	}
	for i := 1; i < len(arrivals); i++ {
		if gap := arrivals[i].Sub(arrivals[i-1]); gap < 20*time.Millisecond {
			t.Fatalf("unpaced HTTP requests: %s", gap)
		}
	}
	// Use a long interval so cancellation cannot race a short cooldown expiring.
	paced := NewClient(server.URL, Options{MinInterval: time.Hour})
	if err := paced.Call(t.Context(), "eth_chainId", []any{}, nil); err != nil {
		t.Fatal(err)
	}
	// A canceled queued read must never reach the provider.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Millisecond)
	defer cancel()
	if err := paced.Call(ctx, "eth_chainId", []any{}, nil); err == nil {
		t.Fatal("canceled call succeeded")
	}
	if len(arrivals) != 5 {
		t.Fatal("canceled read reached provider")
	}
}

// Submission endpoints are separate hosts; a read provider's pacing or
// throttle must never delay a signed send.
func TestIsolatedClientIgnoresSharedPacing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": 1, "result": "0xa"})
	}))
	defer server.Close()
	shared := NewRequestPolicy(Options{MinInterval: time.Hour})
	shared.coolDown(time.Hour)
	ctx, cancel := context.WithTimeout(WithRequestPolicy(t.Context(), shared), time.Second)
	defer cancel()
	submit := NewClient(server.URL, Options{Isolated: true})
	for i := 0; i < 2; i++ {
		if err := submit.Call(ctx, "eth_sendRawTransaction", []any{"0x"}, nil); err != nil {
			t.Fatal(err)
		}
	}
}
