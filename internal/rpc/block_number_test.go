package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestBlockByNumberSuccess(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	parent := mustHash(t, "0x"+strings.Repeat("cd", 32))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		if request.Method != "eth_getBlockByNumber" {
			t.Errorf("method = %q, want eth_getBlockByNumber", request.Method)
		}
		if len(request.Params) != 2 {
			t.Errorf("len(params) = %d, want 2", len(request.Params))
		} else if request.Params[0] != "0x2a" || request.Params[1] != false {
			t.Errorf("params = %v, want [\"0x2a\", false]", request.Params)
		}
		writeResult(t, w, request.ID, map[string]any{
			"number":        "0x2a",
			"hash":          hash.Hex(),
			"parentHash":    parent.Hex(),
			"timestamp":     "0x10",
			"l1BlockNumber": "0x1f4",
		})
	}))
	defer server.Close()

	block, err := NewClient(server.URL, Options{}).BlockByNumber(context.Background(), 42)
	if err != nil {
		t.Fatalf("BlockByNumber: %v", err)
	}
	want := Block{Number: 42, Hash: hash, ParentHash: parent, Timestamp: 16, L1BlockNumber: 500}
	if block != want {
		t.Fatalf("BlockByNumber = %+v, want %+v", block, want)
	}
}

func TestBlockByNumberNullResult(t *testing.T) {
	server := resultServer(t, nil)

	_, err := NewClient(server.URL, Options{}).BlockByNumber(context.Background(), 42)
	if !errors.Is(err, domain.ErrNotFound) || !errors.Is(err, ErrBlockUnavailable) {
		t.Fatalf("error = %v, want domain.ErrNotFound", err)
	}
}

func TestBlockByNumberMalformed(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	parent := mustHash(t, "0x"+strings.Repeat("cd", 32))
	validResult := func() map[string]any {
		return map[string]any{
			"number":        "0x2a",
			"hash":          hash.Hex(),
			"parentHash":    parent.Hex(),
			"timestamp":     "0x10",
			"l1BlockNumber": "0x1f4",
		}
	}

	tests := []struct {
		name     string
		result   any
		mutate   func(map[string]any)
		wantErr  error
		wantText string
	}{
		{
			name:    "missing l1BlockNumber",
			mutate:  func(result map[string]any) { delete(result, "l1BlockNumber") },
			wantErr: domain.ErrNotFound,
		},
		{
			name:     "malformed number",
			mutate:   func(result map[string]any) { result["number"] = "0xzz" },
			wantText: "number",
		},
		{
			name:     "malformed hash",
			mutate:   func(result map[string]any) { result["hash"] = "0x12" },
			wantErr:  domain.ErrInvalidHash,
			wantText: "hash",
		},
		{
			name:     "number mismatch",
			mutate:   func(result map[string]any) { result["number"] = "0x2b" },
			wantText: "node returned number 43",
		},
		{
			name:     "result is not a header",
			result:   "0x2a",
			wantText: "decode header",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := test.result
			if test.mutate != nil {
				header := validResult()
				test.mutate(header)
				result = header
			}
			server := resultServer(t, result)

			_, err := NewClient(server.URL, Options{}).BlockByNumber(context.Background(), 42)
			if err == nil {
				t.Fatal("BlockByNumber succeeded, want error")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
			if test.wantText != "" && !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("error = %q, want text %q", err, test.wantText)
			}
		})
	}
}

func TestBlockByTagSuccess(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	parent := mustHash(t, "0x"+strings.Repeat("cd", 32))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		if request.Method != "eth_getBlockByNumber" {
			t.Errorf("method = %q, want eth_getBlockByNumber", request.Method)
		}
		if len(request.Params) != 2 {
			t.Errorf("len(params) = %d, want 2", len(request.Params))
		} else if request.Params[0] != "latest" || request.Params[1] != false {
			t.Errorf("params = %v, want [\"latest\", false]", request.Params)
		}
		writeResult(t, w, request.ID, map[string]any{
			"number":        "0x2a",
			"hash":          hash.Hex(),
			"parentHash":    parent.Hex(),
			"timestamp":     "0x10",
			"l1BlockNumber": "0x1f4",
		})
	}))
	defer server.Close()

	block, err := NewClient(server.URL, Options{}).BlockByTag(context.Background(), "latest")
	if err != nil {
		t.Fatalf("BlockByTag: %v", err)
	}
	want := Block{Number: 42, Hash: hash, ParentHash: parent, Timestamp: 16, L1BlockNumber: 500}
	if block != want {
		t.Fatalf("BlockByTag = %+v, want %+v", block, want)
	}
}

func TestLatestBlockUsesLatestTag(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		if len(request.Params) != 2 || request.Params[0] != "latest" {
			t.Errorf("params = %v, want the latest tag", request.Params)
		}
		writeResult(t, w, request.ID, map[string]any{
			"number":        "0x2a",
			"hash":          hash.Hex(),
			"parentHash":    hash.Hex(),
			"timestamp":     "0x10",
			"l1BlockNumber": "0x1f4",
		})
	}))
	defer server.Close()

	if _, err := NewClient(server.URL, Options{}).LatestBlock(context.Background()); err != nil {
		t.Fatalf("LatestBlock: %v", err)
	}
}

func TestBlockByTagRejectsEmptyTag(t *testing.T) {
	server := resultServer(t, nil)
	_, err := NewClient(server.URL, Options{}).BlockByTag(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "empty block tag") {
		t.Fatalf("error = %v, want an empty-tag error", err)
	}
}

func TestBlockByTagNullResult(t *testing.T) {
	server := resultServer(t, nil)

	_, err := NewClient(server.URL, Options{}).BlockByTag(context.Background(), "finalized")
	if !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("error = %v, want domain.ErrNotFound", err)
	}
}

func TestBlockByNumberRPCError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"error":   map[string]any{"code": -32000, "message": "header not found"},
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	defer server.Close()

	_, err := NewClient(server.URL, Options{}).BlockByNumber(context.Background(), 42)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error = %v, want *RPCError", err)
	}
	if rpcErr.Code != -32000 {
		t.Fatalf("code = %d, want -32000", rpcErr.Code)
	}
}
