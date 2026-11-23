package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderJSONThrottleIsRetriedWithoutTreatingAllRPCErrorsAsTransient(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&q)
		row := map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": "0xa"}
		if attempts.Add(1) == 1 {
			delete(row, "result")
			row["error"] = map[string]any{"code": -32000, "message": "you have exceeded the rate limit"}
		}
		json.NewEncoder(w).Encode(row)
	}))
	defer server.Close()
	client := NewClient(server.URL, Options{MaxRetries: 1})
	var value string
	if err := client.Call(t.Context(), "eth_chainId", []any{}, &value); err != nil {
		t.Fatal(err)
	}
	if value != "0xa" || attempts.Load() != 2 {
		t.Fatal(value, attempts.Load())
	}
}

func TestBatchPacingChargesEachMethod(t *testing.T) {
	var arrivals []time.Time
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		arrivals = append(arrivals, time.Now())
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		if raw[0] == '[' {
			var calls []struct{ ID uint64 }
			json.Unmarshal(raw, &calls)
			rows := []any{}
			for _, q := range calls {
				rows = append(rows, map[string]any{"id": q.ID, "result": "ok"})
			}
			json.NewEncoder(w).Encode(rows)
			return
		}
		var q struct{ ID uint64 }
		json.Unmarshal(raw, &q)
		json.NewEncoder(w).Encode(map[string]any{"id": q.ID, "result": "ok"})
	}))
	defer server.Close()
	client := NewClient(server.URL, Options{MinInterval: 30 * time.Millisecond})
	if _, err := client.Batch(t.Context(), []Call{{Method: "eth_chainId"}, {Method: "eth_chainId"}, {Method: "eth_chainId"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(t.Context(), "eth_chainId", nil, nil); err != nil {
		t.Fatal(err)
	}
	if gap := arrivals[1].Sub(arrivals[0]); gap < 75*time.Millisecond {
		t.Fatal("batch bypassed method pacing", gap)
	}
}

func TestReadRecoverySurvivesBoundedAttemptsAndHonorsShutdown(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"id": 1, "result": "0xa"})
	}))
	defer server.Close()
	client := NewClient(server.URL, Options{RetryReads: true})
	if err := client.Call(t.Context(), "eth_chainId", nil, nil); err != nil {
		t.Fatal(err)
	}
	if attempts.Load() != 2 {
		t.Fatal(attempts.Load())
	}

	limited := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer limited.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if err := NewClient(limited.URL, Options{RetryReads: true}).Call(ctx, "eth_blockNumber", nil, nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("provider cooldown did not honor shutdown", err)
	}
}

func TestReadRecoveryCannotIndefinitelyResendTransactions(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	err := NewClient(server.URL, Options{RetryReads: true}).Call(t.Context(), "eth_sendRawTransaction", []any{"0x01"}, nil)
	if err == nil || attempts.Load() != 1 {
		t.Fatal("write acquired unbounded retry policy", attempts.Load(), err)
	}
}

func TestRevertMentioningRateLimitIsNotAProviderThrottle(t *testing.T) {
	if IsRateLimited(&RPCError{Code: -32000, Message: "execution reverted: rate limit"}) {
		t.Fatal("contract revert classified as provider throttling")
	}
}

// Chainstack's plan limit names neither "rate" nor "requests".
func TestPlanRPSLimitIsAProviderThrottle(t *testing.T) {
	err := &RPCError{Code: -32005, Message: "You've exceeded the RPS limit available on the current plan. Learn more how to increase the limit"}
	if !IsRateLimited(err) {
		t.Fatal("plan RPS limit not classified as provider throttling")
	}
}

func TestStatefulFilterReadsDoNotAcquireUnboundedRetries(t *testing.T) {
	if readOnlyPayload([]byte(`{"method":"eth_getFilterChanges"}`)) {
		t.Fatal("stateful filter cursor was considered an idempotent read")
	}
}

func TestResponsesReportMethodThrottleAndFailure(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var q struct{ ID uint64 }
		json.NewDecoder(r.Body).Decode(&q)
		switch attempts.Add(1) {
		case 1:
			w.WriteHeader(http.StatusTooManyRequests)
		case 2:
			w.WriteHeader(http.StatusBadGateway)
		default:
			json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": q.ID, "result": "0xa"})
		}
	}))
	defer server.Close()
	var mu sync.Mutex
	var seen []ResponseObservation
	client := NewClient(server.URL, Options{MaxRetries: 2, ObserveResponse: func(o ResponseObservation) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, o)
	}})
	var value string
	if err := client.Call(t.Context(), "eth_chainId", []any{}, &value); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0].Method != "eth_chainId" || !seen[0].Throttled || !seen[0].Failed ||
		seen[1].Throttled || !seen[1].Failed || seen[2].Failed || seen[2].Elapsed <= 0 {
		t.Fatalf("observations: %+v", seen)
	}
}

// A load-balanced endpoint spreads calls over backends that reach a new block
// at different times.
func TestLaggingBackendIsBlockUnavailable(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{&RPCError{Code: -32000, Message: "requested block ahead of current block and the hash is not currently canonical"}, true},
		{&RPCError{Code: -32000, Message: "header not found"}, true},
		{&RPCError{Code: -32000, Message: "unknown block"}, true},
		{&RPCError{Code: 3, Message: "execution reverted"}, false},
		{&RPCError{Code: -32000, Message: "missing trie node"}, false},
	} {
		if got := IsBlockUnavailable(tc.err); got != tc.want {
			t.Errorf("%v: got %v", tc.err, got)
		}
	}
}

// Alchemy throttles single members of a batch with code 429. The
// answered members stay; the throttled one is sent again alone.
func TestBatchResendsOnlyThrottledReadMembers(t *testing.T) {
	var requests [][]rpcRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch []rpcRequest
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		requests = append(requests, batch)
		var rows []map[string]any
		for i, call := range batch {
			if len(requests) == 1 && i == 1 {
				rows = append(rows, map[string]any{"jsonrpc": "2.0", "id": call.ID, "error": map[string]any{"code": 429, "message": "Your app has exceeded its compute units per second capacity."}})
				continue
			}
			rows = append(rows, map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": fmt.Sprintf("0x%x", len(requests))})
		}
		json.NewEncoder(w).Encode(rows)
	}))
	defer server.Close()
	client := NewClient(server.URL, Options{RetryReads: true})
	results, err := client.Batch(t.Context(), []Call{{Method: "eth_call"}, {Method: "eth_call"}, {Method: "eth_getBalance"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(requests) != 2 || len(requests[1]) != 1 {
		t.Fatalf("requests %d, second with %d members", len(requests), len(requests[1]))
	}
	for i, want := range []string{`"0x1"`, `"0x2"`, `"0x1"`} {
		if results[i].Error != nil || string(results[i].Result) != want {
			t.Fatalf("member %d = %s %v, want %s", i, results[i].Result, results[i].Error, want)
		}
	}
}

// A backend whose state trails the block answers "layer stale" (Alchemy);
// the read is sent again a few times, then the error stands.
func TestBatchResendsStaleBackendReadsAFewTimes(t *testing.T) {
	for _, tc := range []struct {
		stale, requests int
		ok              bool
	}{{stale: 2, requests: 3, ok: true}, {stale: 10, requests: 1 + staleReadRetries}} {
		var requests int
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var batch []rpcRequest
			if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
				t.Error(err)
				return
			}
			requests++
			row := map[string]any{"jsonrpc": "2.0", "id": batch[0].ID, "result": "0x1"}
			if requests <= tc.stale {
				row = map[string]any{"jsonrpc": "2.0", "id": batch[0].ID, "error": map[string]any{"code": -32000, "message": "layer stale"}}
			}
			json.NewEncoder(w).Encode([]any{row})
		}))
		client := NewClient(server.URL, Options{RetryReads: true})
		results, err := client.Batch(t.Context(), []Call{{Method: "eth_call"}})
		server.Close()
		if err != nil || requests != tc.requests || (results[0].Error == nil) != tc.ok {
			t.Fatalf("stale %d: %d requests, error %v %v", tc.stale, requests, err, results[0].Error)
		}
	}
}
