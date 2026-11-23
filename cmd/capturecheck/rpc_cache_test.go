package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

var (
	cachedBlockHash  = "0x" + strings.Repeat("ab", 32)
	cachedParentHash = "0x" + strings.Repeat("cd", 32)
	cachedTxHash     = "0x" + strings.Repeat("11", 32)
	cachedLogAddress = "0x" + strings.Repeat("aa", 20)
	cachedTopic      = "0x" + strings.Repeat("01", 32)
)

// rpcCacheFixture answers the block and receipt lookups the cache tests make
// and counts them by method.
type rpcCacheFixture struct {
	server *httptest.Server
	calls  map[string]*atomic.Int64
}

func newRPCCacheFixture(t *testing.T, results map[string]any) *rpcCacheFixture {
	t.Helper()
	fixture := &rpcCacheFixture{
		calls: map[string]*atomic.Int64{
			"eth_getBlockByNumber": new(atomic.Int64),
			"eth_getBlockReceipts": new(atomic.Int64),
		},
	}
	fixture.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			ID     uint64 `json:"id"`
			Method string `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if counter, ok := fixture.calls[request.Method]; ok {
			counter.Add(1)
		}
		result, ok := results[request.Method]
		if !ok {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      request.ID,
			"result":  result,
		}); err != nil {
			t.Errorf("encode response: %v", err)
		}
	}))
	t.Cleanup(fixture.server.Close)
	return fixture
}

func (f *rpcCacheFixture) callsTo(method string) int64 {
	return f.calls[method].Load()
}

func cachedBlockResult() map[string]any {
	return map[string]any{
		"number":        "0x2a",
		"hash":          cachedBlockHash,
		"parentHash":    cachedParentHash,
		"timestamp":     "0x10",
		"l1BlockNumber": "0x1f4",
	}
}

func cachedReceiptsResult() []any {
	return []any{
		map[string]any{
			"transactionHash": cachedTxHash,
			"logs": []any{
				map[string]any{
					"address":  cachedLogAddress,
					"topics":   []string{cachedTopic},
					"data":     "0xdeadbeef",
					"logIndex": "0x0",
				},
			},
		},
	}
}

func TestCachedBlockByNumberServesRepeatedReadFromDisk(t *testing.T) {
	ctx := context.Background()
	fixture := newRPCCacheFixture(t, map[string]any{"eth_getBlockByNumber": cachedBlockResult()})
	client := rpc.NewClient(fixture.server.URL, rpc.Options{})
	cache := newCachedCaller(rpcCaller{client: client}, t.TempDir())

	first, err := cachedBlockByNumber(ctx, client, cache, 42)
	if err != nil {
		t.Fatalf("first cachedBlockByNumber: %v", err)
	}
	second, err := cachedBlockByNumber(ctx, client, cache, 42)
	if err != nil {
		t.Fatalf("cached cachedBlockByNumber: %v", err)
	}
	if first != second {
		t.Fatalf("cached block = %+v, want %+v", second, first)
	}
	if got := fixture.callsTo("eth_getBlockByNumber"); got != 1 {
		t.Fatalf("upstream block requests = %d, want 1", got)
	}
	if got := cache.hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if got := cache.misses.Load(); got != 1 {
		t.Fatalf("misses = %d, want 1", got)
	}
}

func TestCachedBlockReceiptsServesRepeatedReadFromDisk(t *testing.T) {
	ctx := context.Background()
	fixture := newRPCCacheFixture(t, map[string]any{"eth_getBlockReceipts": cachedReceiptsResult()})
	client := rpc.NewClient(fixture.server.URL, rpc.Options{})
	cache := newCachedCaller(rpcCaller{client: client}, t.TempDir())
	hash, err := domain.ParseHash(cachedBlockHash)
	if err != nil {
		t.Fatal(err)
	}

	first, err := cachedBlockReceipts(ctx, client, cache, hash)
	if err != nil {
		t.Fatalf("first cachedBlockReceipts: %v", err)
	}
	second, err := cachedBlockReceipts(ctx, client, cache, hash)
	if err != nil {
		t.Fatalf("cached cachedBlockReceipts: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("cached receipts = %+v, want %+v", second, first)
	}
	if got := fixture.callsTo("eth_getBlockReceipts"); got != 1 {
		t.Fatalf("upstream receipt requests = %d, want 1", got)
	}
	if got := cache.hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if got := cache.misses.Load(); got != 1 {
		t.Fatalf("misses = %d, want 1", got)
	}
}

func TestCachedBlockReadsForwardWhenCacheDisabled(t *testing.T) {
	ctx := context.Background()
	fixture := newRPCCacheFixture(t, map[string]any{
		"eth_getBlockByNumber": cachedBlockResult(),
		"eth_getBlockReceipts": cachedReceiptsResult(),
	})
	client := rpc.NewClient(fixture.server.URL, rpc.Options{})
	cache := newCachedCaller(rpcCaller{client: client}, "")
	hash, err := domain.ParseHash(cachedBlockHash)
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 2; i++ {
		if _, err := cachedBlockByNumber(ctx, client, cache, 42); err != nil {
			t.Fatalf("cachedBlockByNumber %d: %v", i, err)
		}
		if _, err := cachedBlockReceipts(ctx, client, cache, hash); err != nil {
			t.Fatalf("cachedBlockReceipts %d: %v", i, err)
		}
	}
	if got := fixture.callsTo("eth_getBlockByNumber"); got != 2 {
		t.Fatalf("upstream block requests = %d, want 2", got)
	}
	if got := fixture.callsTo("eth_getBlockReceipts"); got != 2 {
		t.Fatalf("upstream receipt requests = %d, want 2", got)
	}
	if got := cache.summary(); got != "cache disabled" {
		t.Fatalf("summary = %q, want cache disabled", got)
	}
}

func TestCachedBlockByNumberDoesNotCacheNullBlock(t *testing.T) {
	ctx := context.Background()
	fixture := newRPCCacheFixture(t, map[string]any{"eth_getBlockByNumber": nil})
	client := rpc.NewClient(fixture.server.URL, rpc.Options{})
	cache := newCachedCaller(rpcCaller{client: client}, t.TempDir())

	for i := 0; i < 2; i++ {
		if _, err := cachedBlockByNumber(ctx, client, cache, 42); !errors.Is(err, domain.ErrNotFound) {
			t.Fatalf("cachedBlockByNumber %d error = %v, want domain.ErrNotFound", i, err)
		}
	}
	if got := fixture.callsTo("eth_getBlockByNumber"); got != 2 {
		t.Fatalf("upstream block requests = %d, want 2", got)
	}
}
