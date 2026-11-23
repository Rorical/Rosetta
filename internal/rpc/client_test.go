package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCallSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		var request struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      uint64          `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if request.JSONRPC != "2.0" || request.Method != "eth_blockNumber" {
			t.Errorf("request = %+v, want JSON-RPC 2.0 eth_blockNumber", request)
		}
		if string(request.Params) != `["0xabc"]` {
			t.Errorf("params = %s, want [\"0xabc\"]", request.Params)
		}
		writeResult(t, w, request.ID, "0x2a")
	}))
	defer server.Close()

	client := NewClient(server.URL, Options{})
	var result string
	if err := client.Call(context.Background(), "eth_blockNumber", []string{"0xabc"}, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result != "0x2a" {
		t.Fatalf("result = %q, want 0x2a", result)
	}
}

func TestCallRPCErrorIsNotRetried(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"jsonrpc":"2.0","id":1,"error":{"code":-32003,"message":"conditional transaction rejected","data":{"reason":"condition"}}}`)
	}))
	defer server.Close()

	client := NewClient(server.URL, Options{MaxRetries: 3, Backoff: time.Millisecond})
	err := client.Call(context.Background(), "eth_sendRawTransactionConditional", nil, nil)
	if err == nil {
		t.Fatal("Call succeeded, want JSON-RPC error")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v (%T), want *RPCError", err, err)
	}
	if rpcErr.Code != -32003 || rpcErr.Message != "conditional transaction rejected" {
		t.Fatalf("RPCError = %+v, want code -32003 with server message", rpcErr)
	}
	if string(rpcErr.Data) != `{"reason":"condition"}` {
		t.Fatalf("Data = %s, want condition payload", rpcErr.Data)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1 (no retry on JSON-RPC error)", attempts.Load())
	}
}

func TestCallRetriesRetryableStatus(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			var attempts atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if attempts.Add(1) == 1 {
					w.WriteHeader(status)
					io.WriteString(w, "temporarily unavailable")
					return
				}
				writeResult(t, w, 1, "ok")
			}))
			defer server.Close()

			client := NewClient(server.URL, Options{MaxRetries: 2, Backoff: time.Millisecond})
			var result string
			if err := client.Call(context.Background(), "eth_blockNumber", nil, &result); err != nil {
				t.Fatalf("Call: %v", err)
			}
			if result != "ok" {
				t.Fatalf("result = %q, want ok", result)
			}
			if attempts.Load() != 2 {
				t.Fatalf("attempts = %d, want 2", attempts.Load())
			}
		})
	}
}

func TestCallRetryExhaustion(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusBadGateway)
		io.WriteString(w, "bad gateway")
	}))
	defer server.Close()

	client := NewClient(server.URL, Options{MaxRetries: 2, Backoff: time.Millisecond})
	err := client.Call(context.Background(), "eth_blockNumber", nil, nil)
	if err == nil {
		t.Fatal("Call succeeded, want error after retries")
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
	if got := err.Error(); !strings.Contains(got, "502") || !strings.Contains(got, "3 attempts") {
		t.Fatalf("error = %q, want HTTP 502 and attempt count", got)
	}
}

func TestCallRetriesTransportError(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Errorf("response does not support hijacking")
				return
			}
			connection, _, err := hijacker.Hijack()
			if err != nil {
				t.Errorf("hijack: %v", err)
				return
			}
			connection.Close()
			return
		}
		writeResult(t, w, 1, "ok")
	}))
	defer server.Close()

	client := NewClient(server.URL, Options{MaxRetries: 2, Backoff: time.Millisecond})
	var result string
	if err := client.Call(context.Background(), "eth_blockNumber", nil, &result); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if result != "ok" {
		t.Fatalf("result = %q, want ok", result)
	}
	if attempts.Load() != 2 {
		t.Fatalf("attempts = %d, want 2", attempts.Load())
	}
}

func TestBatchMapsResultsByID(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var requests []struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&requests); err != nil {
			t.Errorf("decode batch request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(requests) != 3 {
			t.Errorf("len(requests) = %d, want 3", len(requests))
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		wantMethods := []string{"first", "second", "third"}
		for i, request := range requests {
			if request.Method != wantMethods[i] {
				t.Errorf("request %d method = %q, want %q", i, request.Method, wantMethods[i])
			}
		}

		// Respond out of order; the client must map ids back to call order.
		responses := []any{
			map[string]any{"jsonrpc": "2.0", "id": requests[2].ID, "result": "third"},
			map[string]any{"jsonrpc": "2.0", "id": requests[1].ID, "error": map[string]any{"code": -32601, "message": "method not found"}},
			map[string]any{"jsonrpc": "2.0", "id": requests[0].ID, "result": "first"},
		}
		if err := json.NewEncoder(w).Encode(responses); err != nil {
			t.Errorf("encode batch response: %v", err)
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, Options{})
	results, err := client.Batch(context.Background(), []Call{
		{Method: "first"},
		{Method: "second"},
		{Method: "third"},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("len(results) = %d, want 3", len(results))
	}
	if string(results[0].Result) != `"first"` {
		t.Fatalf("results[0] = %s, want \"first\"", results[0].Result)
	}
	if results[1].Error == nil || results[1].Error.Code != -32601 {
		t.Fatalf("results[1].Error = %+v, want code -32601", results[1].Error)
	}
	if string(results[2].Result) != `"third"` {
		t.Fatalf("results[2] = %s, want \"third\"", results[2].Result)
	}
}

func TestBatchEmptyDoesNotCallServer(t *testing.T) {
	client := NewClient("http://127.0.0.1:1", Options{})
	results, err := client.Batch(context.Background(), nil)
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(results))
	}
}

func TestCallContextCancel(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, Options{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-started
		cancel()
	}()

	err := client.Call(ctx, "slow_method", nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func TestCallClientTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(100 * time.Millisecond):
		}
	}))
	defer server.Close()

	client := NewClient(server.URL, Options{Timeout: 20 * time.Millisecond})
	if err := client.Call(context.Background(), "slow_method", nil, nil); err == nil {
		t.Fatal("Call succeeded, want timeout error")
	}
}

func TestRedactURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "host only", raw: "https://rpc.example", want: "https://rpc.example"},
		{name: "port", raw: "http://127.0.0.1:8545/v2/SECRET", want: "http://127.0.0.1:8545"},
		{
			name: "userinfo path query fragment",
			raw:  "https://user:pass@rpc.example/v2/SECRET?api_key=TOKEN#frag",
			want: "https://rpc.example",
		},
		{name: "invalid", raw: "://bad", want: "<rpc endpoint>"},
		{name: "relative", raw: "/v2/SECRET", want: "<rpc endpoint>"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := redactURL(test.raw); got != test.want {
				t.Fatalf("redactURL(%q) = %q, want %q", test.raw, got, test.want)
			}
		})
	}
}

// TestCallErrorsHideEndpointSecrets checks every error path that prints a
// request: a transport failure and an HTTP status failure. The endpoint path
// and query both carry a secret.
func TestCallErrorsHideEndpointSecrets(t *testing.T) {
	const (
		pathSecret  = "provider-path-secret"
		querySecret = "provider-query-secret"
	)
	tests := []struct {
		name         string
		handler      http.HandlerFunc
		wantEndpoint bool
	}{
		{
			name:         "transport failure",
			wantEndpoint: true,
			handler: func(w http.ResponseWriter, r *http.Request) {
				hijacker, ok := w.(http.Hijacker)
				if !ok {
					t.Errorf("response does not support hijacking")
					return
				}
				connection, _, err := hijacker.Hijack()
				if err != nil {
					t.Errorf("hijack: %v", err)
					return
				}
				connection.Close()
			},
		},
		{
			name: "http status failure",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				io.WriteString(w, "upstream failed")
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(test.handler)
			defer server.Close()

			endpoint := server.URL + "/v2/" + pathSecret + "?api_key=" + querySecret
			client := NewClient(endpoint, Options{Timeout: time.Second})
			err := client.Call(context.Background(), "eth_blockNumber", nil, nil)
			if err == nil {
				t.Fatal("Call succeeded, want an error")
			}
			text := err.Error()
			for _, secret := range []string{pathSecret, querySecret, "/v2/", "api_key="} {
				if strings.Contains(text, secret) {
					t.Fatalf("error %q contains endpoint part %q", text, secret)
				}
			}
			if test.wantEndpoint && !strings.Contains(text, server.URL) {
				t.Fatalf("error %q does not name the redacted endpoint %q", text, server.URL)
			}
		})
	}
}

func writeResult(t *testing.T, w http.ResponseWriter, id uint64, result any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"result":  result,
	}); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

// A pinned client dials its DialAddress while the request keeps the URL's
// name, so one name can front several addresses.
func TestCallDialsPinnedAddress(t *testing.T) {
	var host string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host = r.Host
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":"0x1"}`))
	}))
	defer server.Close()
	client := NewClient("http://sequencer.invalid:8547", Options{DialAddress: strings.TrimPrefix(server.URL, "http://")})
	var out string
	if err := client.Call(t.Context(), "eth_chainId", []any{}, &out); err != nil || out != "0x1" {
		t.Fatal(out, err)
	}
	if host != "sequencer.invalid:8547" {
		t.Fatal("host header", host)
	}
}
