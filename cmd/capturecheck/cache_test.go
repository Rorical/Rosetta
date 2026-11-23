package main

import (
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// countingCaller records how many reads reach the "node". Its results encode
// the calldata so a test can tell which call produced which value; empty
// calldata yields "0x", the empty-bytes result.
type countingCaller struct {
	single  int
	batch   int
	batched []pool.ContractCall
}

func (c *countingCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	c.single++
	*out = resultFor(data)
	return nil
}

func (c *countingCaller) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	c.batch++
	c.batched = append(c.batched, calls...)
	out := make([]string, len(calls))
	for i, call := range calls {
		out[i] = resultFor(call.Data)
	}
	return out, nil
}

func resultFor(data []byte) string {
	return "0x" + hex.EncodeToString(data)
}

func TestCachedCallerServesRepeatedCallFromDisk(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "cache")
	cache := newCachedCaller(&countingCaller{}, dir)
	to := testAddress(0xaa)

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("cache dir exists before first successful call: %v", err)
	}

	var first, second string
	if err := cache.CallContract(ctx, to, []byte{0x01}, "0x10", &first); err != nil {
		t.Fatalf("first CallContract: %v", err)
	}
	if err := cache.CallContract(ctx, to, []byte{0x01}, "0x10", &second); err != nil {
		t.Fatalf("second CallContract: %v", err)
	}
	if first != "0x01" || second != "0x01" {
		t.Fatalf("results = %q/%q, want 0x01/0x01", first, second)
	}
	if got := cache.under.(*countingCaller).single; got != 1 {
		t.Fatalf("underlying single calls = %d, want 1", got)
	}
	if got := cache.hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if got := cache.misses.Load(); got != 1 {
		t.Fatalf("misses = %d, want 1", got)
	}

	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("cache dir not created: %v", err)
	}
	if got := cache.summary(); got != "cache hits=1 misses=1" {
		t.Fatalf("summary = %q", got)
	}
}

func TestCachedCallerSeparatesBlocks(t *testing.T) {
	ctx := context.Background()
	cache := newCachedCaller(&countingCaller{}, t.TempDir())
	to := testAddress(0xaa)

	var out string
	if err := cache.CallContract(ctx, to, []byte{0x01}, "0x10", &out); err != nil {
		t.Fatalf("CallContract block 0x10: %v", err)
	}
	if err := cache.CallContract(ctx, to, []byte{0x01}, "0x11", &out); err != nil {
		t.Fatalf("CallContract block 0x11: %v", err)
	}
	if got := cache.under.(*countingCaller).single; got != 2 {
		t.Fatalf("underlying single calls = %d, want 2", got)
	}
}

func TestCachedCallerForwardsOnlyBatchMisses(t *testing.T) {
	ctx := context.Background()
	cache := newCachedCaller(&countingCaller{}, t.TempDir())
	to := testAddress(0xaa)
	cached := pool.ContractCall{To: to, Data: []byte{0xaa}, Block: "0x10"}
	missing := pool.ContractCall{To: to, Data: []byte{0xbb}, Block: "0x10"}

	var primed string
	if err := cache.CallContract(ctx, cached.To, cached.Data, cached.Block, &primed); err != nil {
		t.Fatalf("prime cache: %v", err)
	}

	results, err := cache.CallContracts(ctx, []pool.ContractCall{cached, missing})
	if err != nil {
		t.Fatalf("CallContracts: %v", err)
	}
	if len(results) != 2 || results[0] != "0xaa" || results[1] != "0xbb" {
		t.Fatalf("results = %v, want [0xaa 0xbb]", results)
	}

	fake := cache.under.(*countingCaller)
	if fake.batch != 1 {
		t.Fatalf("underlying batches = %d, want 1", fake.batch)
	}
	if len(fake.batched) != 1 {
		t.Fatalf("forwarded calls = %d, want 1", len(fake.batched))
	}
	if got := fake.batched[0]; got.To != missing.To || string(got.Data) != string(missing.Data) || got.Block != missing.Block {
		t.Fatalf("forwarded call = %+v, want %+v", got, missing)
	}
	if got := cache.hits.Load(); got != 1 {
		t.Fatalf("hits = %d, want 1", got)
	}
	if got := cache.misses.Load(); got != 2 {
		t.Fatalf("misses = %d, want 2", got)
	}

	// A fully cached batch must not reach the node at all.
	if _, err := cache.CallContracts(ctx, []pool.ContractCall{cached, missing}); err != nil {
		t.Fatalf("cached CallContracts: %v", err)
	}
	if fake.batch != 1 {
		t.Fatalf("underlying batches after cached batch = %d, want 1", fake.batch)
	}
}

func TestCachedCallerCachesEmptyBytes(t *testing.T) {
	ctx := context.Background()
	cache := newCachedCaller(&countingCaller{}, t.TempDir())

	var first, second string
	if err := cache.CallContract(ctx, testAddress(0xaa), nil, "0x10", &first); err != nil {
		t.Fatalf("first CallContract: %v", err)
	}
	if err := cache.CallContract(ctx, testAddress(0xaa), nil, "0x10", &second); err != nil {
		t.Fatalf("second CallContract: %v", err)
	}
	if first != "0x" || second != "0x" {
		t.Fatalf("results = %q/%q, want 0x/0x", first, second)
	}
	if got := cache.under.(*countingCaller).single; got != 1 {
		t.Fatalf("underlying single calls = %d, want 1", got)
	}
}

func TestCachedCallerWithoutDirectoryAlwaysForwards(t *testing.T) {
	ctx := context.Background()
	fake := &countingCaller{}
	cache := newCachedCaller(fake, "")
	to := testAddress(0xaa)

	var out string
	if err := cache.CallContract(ctx, to, []byte{0x01}, "0x10", &out); err != nil {
		t.Fatalf("first CallContract: %v", err)
	}
	if err := cache.CallContract(ctx, to, []byte{0x01}, "0x10", &out); err != nil {
		t.Fatalf("second CallContract: %v", err)
	}
	if fake.single != 2 {
		t.Fatalf("underlying single calls = %d, want 2", fake.single)
	}

	calls := []pool.ContractCall{
		{To: to, Data: []byte{0x02}, Block: "0x10"},
		{To: to, Data: []byte{0x03}, Block: "0x10"},
	}
	if _, err := cache.CallContracts(ctx, calls); err != nil {
		t.Fatalf("CallContracts: %v", err)
	}
	if len(fake.batched) != 2 {
		t.Fatalf("forwarded calls = %d, want 2", len(fake.batched))
	}
	if got := cache.summary(); got != "cache disabled" {
		t.Fatalf("summary = %q, want cache disabled", got)
	}
}

func TestCachedCallerWriteFailureDoesNotFailCalls(t *testing.T) {
	ctx := context.Background()
	// A regular file where the cache directory should be makes MkdirAll fail.
	blocked := filepath.Join(t.TempDir(), "blocked")
	if err := os.WriteFile(blocked, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("prepare blocked path: %v", err)
	}
	fake := &countingCaller{}
	cache := newCachedCaller(fake, blocked)

	var out string
	if err := cache.CallContract(ctx, testAddress(0xaa), []byte{0x01}, "0x10", &out); err != nil {
		t.Fatalf("CallContract: %v", err)
	}
	if out != "0x01" {
		t.Fatalf("result = %q, want 0x01", out)
	}
	if cache.disabledReason() == "" {
		t.Fatal("cache did not report the write failure")
	}
	if got := cache.summary(); !strings.Contains(got, "write_errors=1") {
		t.Fatalf("summary = %q, want a write_errors count", got)
	}

	if err := cache.CallContract(ctx, testAddress(0xaa), []byte{0x02}, "0x10", &out); err != nil {
		t.Fatalf("second CallContract: %v", err)
	}
	if fake.single != 2 {
		t.Fatalf("underlying single calls = %d, want 2", fake.single)
	}
}
