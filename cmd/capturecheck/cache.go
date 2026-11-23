package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// cacheVersion prefixes every cache key. Bump it when the key or value format
// changes so old entries miss instead of being misread.
const cacheVersion = "v1"

// ethCaller is the read-only call surface the cache wraps. Both halves are
// needed because the pool loaders switch to their batch path only when the
// caller also implements BatchContractCaller.
type ethCaller interface {
	pool.ContractCaller
	pool.BatchContractCaller
}

// cachedCaller memoizes historical RPC reads in a directory: raw eth_call
// results and the parsed headers and receipts of the case's blocks. Historical
// values cannot change, so entries never expire. Writes go through a temporary
// file and a rename, so an interrupted run cannot leave a truncated result that
// a later run would trust.
type cachedCaller struct {
	under ethCaller
	dir   string

	hits   atomic.Int64
	misses atomic.Int64
	// failedWrites counts cache write failures. The first one disables further
	// writes, so this is 0 or 1 in practice; it is separate from the reason so
	// the footer always states the hit and miss counts.
	failedWrites atomic.Int64

	mu       sync.Mutex
	disabled error
}

var (
	_ pool.ContractCaller      = (*cachedCaller)(nil)
	_ pool.BatchContractCaller = (*cachedCaller)(nil)
)

// newCachedCaller wraps under with a cache rooted at dir. An empty dir makes
// every read a miss and every write a no-op, so callers can always wrap and
// report the disabled state instead of branching.
func newCachedCaller(under ethCaller, dir string) *cachedCaller {
	return &cachedCaller{under: under, dir: dir}
}

// defaultCacheDir is the per-user directory for cached historical results. A
// missing user cache directory disables caching rather than failing the run.
func defaultCacheDir() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "rosetta-capture")
}

// CallContract returns the cached result for the call, or forwards it and
// stores the result on success. Errors are never cached.
func (c *cachedCaller) CallContract(ctx context.Context, to domain.Address, data []byte, block string, out *string) error {
	key := cacheKey(block, to, data)
	if raw, ok := c.load(key); ok {
		c.hits.Add(1)
		*out = raw
		return nil
	}
	c.misses.Add(1)

	var raw string
	if err := c.under.CallContract(ctx, to, data, block, &raw); err != nil {
		return err
	}
	c.store(key, raw)
	*out = raw
	return nil
}

// CallContracts serves cached calls locally and forwards only the misses, as
// one underlying batch. Results keep the caller's order.
func (c *cachedCaller) CallContracts(ctx context.Context, calls []pool.ContractCall) ([]string, error) {
	results := make([]string, len(calls))
	misses := make([]int, 0, len(calls))
	missCalls := make([]pool.ContractCall, 0, len(calls))
	for i, call := range calls {
		if raw, ok := c.load(cacheKey(call.Block, call.To, call.Data)); ok {
			c.hits.Add(1)
			results[i] = raw
			continue
		}
		c.misses.Add(1)
		misses = append(misses, i)
		missCalls = append(missCalls, call)
	}
	if len(missCalls) == 0 {
		return results, nil
	}

	fetched, err := c.under.CallContracts(ctx, missCalls)
	if err != nil {
		return nil, err
	}
	for j, i := range misses {
		results[i] = fetched[j]
		c.store(cacheKey(calls[i].Block, calls[i].To, calls[i].Data), fetched[j])
	}
	return results, nil
}

// cachedBlockByNumber returns the canonical header at number, from disk when an
// earlier run resolved it. Errors and null blocks are never cached.
func cachedBlockByNumber(ctx context.Context, client *rpc.Client, cache *cachedCaller, number uint64) (rpc.Block, error) {
	key := entryKey("block-number:" + strconv.FormatUint(number, 10))
	var block rpc.Block
	if cache.loadJSON(key, &block) {
		cache.hits.Add(1)
		return block, nil
	}
	cache.misses.Add(1)

	block, err := client.BlockByNumber(ctx, number)
	if err != nil {
		return rpc.Block{}, err
	}
	cache.storeJSON(key, block)
	return block, nil
}

// cachedBlockReceipts returns every receipt in the block identified by hash,
// from disk when an earlier run resolved it. Errors are never cached.
func cachedBlockReceipts(ctx context.Context, client *rpc.Client, cache *cachedCaller, hash domain.Hash) ([]rpc.Receipt, error) {
	key := entryKey("block-receipts:" + hash.Hex())
	var receipts []rpc.Receipt
	if cache.loadJSON(key, &receipts) {
		cache.hits.Add(1)
		return receipts, nil
	}
	cache.misses.Add(1)

	receipts, err := client.BlockReceipts(ctx, hash)
	if err != nil {
		return nil, err
	}
	cache.storeJSON(key, receipts)
	return receipts, nil
}

// summary is the cache footer line. An empty directory means every call was
// forwarded, so only the disabled state is reported.
func (c *cachedCaller) summary() string {
	if c.dir == "" {
		return "cache disabled"
	}
	line := fmt.Sprintf("cache hits=%d misses=%d", c.hits.Load(), c.misses.Load())
	if failed := c.failedWrites.Load(); failed > 0 {
		line += fmt.Sprintf(" write_errors=%d", failed)
	}
	return line
}

// disabledReason reports the first cache I/O failure, if any.
func (c *cachedCaller) disabledReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled == nil {
		return ""
	}
	return c.disabled.Error()
}

// cacheKey hashes one call identity. block is used exactly as the loader passes
// it, so a different tag for the same history is a different entry.
func cacheKey(block string, to domain.Address, data []byte) string {
	return entryKey(block + "\x00" + to.Hex() + "\x00" + hex.EncodeToString(data))
}

// entryKey hashes one cache entry name under the version prefix, so a format
// bump misses old entries instead of misreading them. The digest is the shard
// and file name.
func entryKey(name string) string {
	sum := sha256.New()
	sum.Write([]byte(cacheVersion))
	sum.Write([]byte{0})
	sum.Write([]byte(name))
	return hex.EncodeToString(sum.Sum(nil))
}

// path places key under a two-character shard so a large cache does not put
// every entry in one directory.
func (c *cachedCaller) path(key string) string {
	return filepath.Join(c.dir, key[:2], key+".hex")
}

// jsonPath places a parsed-value entry next to the eth_call entries. The
// extension differs because the content is JSON, not a hex result.
func (c *cachedCaller) jsonPath(key string) string {
	return filepath.Join(c.dir, key[:2], key+".json")
}

// load returns the cached result for key. A missing directory, a missing file,
// and an empty file all read as a miss.
func (c *cachedCaller) load(key string) (string, bool) {
	if c.dir == "" {
		return "", false
	}
	raw, err := os.ReadFile(c.path(key))
	if err != nil || len(raw) == 0 {
		return "", false
	}
	return string(raw), true
}

// loadJSON decodes the cached entry for key into out. A missing, empty, or
// undecodable entry reads as a miss: the cache is best-effort.
func (c *cachedCaller) loadJSON(key string, out any) bool {
	if c.dir == "" {
		return false
	}
	raw, err := os.ReadFile(c.jsonPath(key))
	if err != nil || len(raw) == 0 {
		return false
	}
	return json.Unmarshal(raw, out) == nil
}

// store caches value under key. An empty string is not a valid eth_call
// result, while "0x" is, so only "" is skipped. Cache failures never fail the
// run: the first one disables further writes and is reported at the end.
func (c *cachedCaller) store(key, value string) {
	if c.dir == "" || value == "" || c.disabledNow() {
		return
	}
	if err := c.writeFile(c.path(key), []byte(value)); err != nil {
		c.failedWrites.Add(1)
		c.disable(err)
	}
}

// storeJSON caches value under key as JSON. Cache failures never fail the run:
// the first one disables further writes and is reported at the end.
func (c *cachedCaller) storeJSON(key string, value any) {
	if c.dir == "" || c.disabledNow() {
		return
	}
	raw, err := json.Marshal(value)
	if err != nil {
		c.failedWrites.Add(1)
		c.disable(fmt.Errorf("encode cache entry %s: %w", key, err))
		return
	}
	if err := c.writeFile(c.jsonPath(key), raw); err != nil {
		c.failedWrites.Add(1)
		c.disable(err)
	}
}

// writeFile writes value atomically at path: a temporary file in the same
// directory, then a rename.
func (c *cachedCaller) writeFile(path string, value []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp file in %s: %w", dir, err)
	}
	name := temp.Name()
	if _, err := temp.Write(value); err != nil {
		temp.Close()
		os.Remove(name)
		return fmt.Errorf("write %s: %w", name, err)
	}
	if err := temp.Close(); err != nil {
		os.Remove(name)
		return fmt.Errorf("close %s: %w", name, err)
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return fmt.Errorf("rename %s: %w", path, err)
	}
	return nil
}

func (c *cachedCaller) disable(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.disabled == nil {
		c.disabled = err
	}
}

func (c *cachedCaller) disabledNow() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.disabled != nil
}
