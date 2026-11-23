package rpc

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestBlockByHashSuccess(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	parent := mustHash(t, "0x"+strings.Repeat("cd", 32))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		request, ok := decodeTestRequest(t, r)
		if !ok {
			return
		}
		if request.Method != "eth_getBlockByHash" {
			t.Errorf("method = %q, want eth_getBlockByHash", request.Method)
		}
		if len(request.Params) != 2 {
			t.Errorf("len(params) = %d, want 2", len(request.Params))
		} else if request.Params[0] != hash.Hex() || request.Params[1] != false {
			t.Errorf("params = %v, want [%q, false]", request.Params, hash.Hex())
		}
		writeResult(t, w, request.ID, map[string]any{
			"number":        "0x2a",
			"hash":          hash.Hex(),
			"parentHash":    parent.Hex(),
			"timestamp":     "0x10",
			"l1BlockNumber": "0x1f4",
			"baseFeePerGas": "0x0",
		})
	}))
	defer server.Close()

	block, err := NewClient(server.URL, Options{}).BlockByHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("BlockByHash: %v", err)
	}
	want := Block{Number: 42, Hash: hash, ParentHash: parent, Timestamp: 16, L1BlockNumber: 500}
	if block != want {
		t.Fatalf("BlockByHash = %+v, want %+v", block, want)
	}
}

func TestBlockByHashNullResult(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	server := resultServer(t, nil)

	_, err := NewClient(server.URL, Options{}).BlockByHash(context.Background(), hash)
	if !errors.Is(err, domain.ErrNotFound) || !errors.Is(err, ErrBlockUnavailable) {
		t.Fatalf("error = %v, want domain.ErrNotFound", err)
	}
}

func TestBlockByHashHashMismatch(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	other := mustHash(t, "0x"+strings.Repeat("cd", 32))
	server := resultServer(t, map[string]any{
		"number":        "0x2a",
		"hash":          other.Hex(),
		"parentHash":    other.Hex(),
		"timestamp":     "0x10",
		"l1BlockNumber": "0x1f4",
	})

	_, err := NewClient(server.URL, Options{}).BlockByHash(context.Background(), hash)
	if err == nil {
		t.Fatal("BlockByHash succeeded, want hash mismatch error")
	}
	if !strings.Contains(err.Error(), "returned hash") {
		t.Fatalf("error = %q, want hash mismatch detail", err)
	}
}

func TestBlockByHashMalformed(t *testing.T) {
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
			name:    "missing timestamp",
			mutate:  func(result map[string]any) { delete(result, "timestamp") },
			wantErr: domain.ErrNotFound,
		},
		{
			name:     "malformed number",
			mutate:   func(result map[string]any) { result["number"] = "0xzz" },
			wantText: "number",
		},
		{
			name:     "number without prefix",
			mutate:   func(result map[string]any) { result["number"] = "42" },
			wantText: "number",
		},
		{
			name:     "malformed timestamp",
			mutate:   func(result map[string]any) { result["timestamp"] = "0x" },
			wantText: "timestamp",
		},
		{
			name:     "malformed l1BlockNumber",
			mutate:   func(result map[string]any) { result["l1BlockNumber"] = "nothex" },
			wantText: "l1BlockNumber",
		},
		{
			name:     "malformed hash",
			mutate:   func(result map[string]any) { result["hash"] = "0x1234" },
			wantErr:  domain.ErrInvalidHash,
			wantText: "hash",
		},
		{
			name:     "malformed parentHash",
			mutate:   func(result map[string]any) { result["parentHash"] = "0x" + strings.Repeat("zz", 32) },
			wantErr:  domain.ErrInvalidHash,
			wantText: "parentHash",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := validResult()
			test.mutate(result)
			server := resultServer(t, result)

			_, err := NewClient(server.URL, Options{}).BlockByHash(context.Background(), hash)
			if err == nil {
				t.Fatal("BlockByHash succeeded, want error")
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

func TestLookupContextCancel(t *testing.T) {
	hash := mustHash(t, "0x"+strings.Repeat("ab", 32))
	tests := []struct {
		name string
		call func(context.Context, *Client) error
	}{
		{
			name: "block",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.BlockByHash(ctx, hash)
				return err
			},
		},
		{
			name: "receipts",
			call: func(ctx context.Context, client *Client) error {
				_, err := client.BlockReceipts(ctx, hash)
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
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

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			go func() {
				<-started
				cancel()
			}()

			if err := test.call(ctx, NewClient(server.URL, Options{})); !errors.Is(err, context.Canceled) {
				t.Fatalf("error = %v, want context.Canceled", err)
			}
		})
	}
}
