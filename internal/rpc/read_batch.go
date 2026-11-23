package rpc

import (
	"context"
	"errors"
	"fmt"
)

// ReadResult preserves successful members of a partially failed immutable batch.
type ReadResult struct {
	Value string
	Err   error
}

// LoadBatch coalesces duplicate and concurrent immutable reads with Load. The
// fetch callback receives only owned misses; successful members survive errors.
func (cache *ReadCache) LoadBatch(ctx context.Context, keys []string, fetch func(context.Context, []string) ([]ReadResult, error)) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	cache.mu.Lock()
	if cache.values == nil {
		cache.values = make(map[string]string)
		cache.pending = make(map[string]*pendingRead)
	}
	values := make([]string, len(keys))
	waiting := make(map[string]*pendingRead)
	owned := []string{}
	produced := make(map[string]bool)
	for i, key := range keys {
		if value, ok := cache.values[key]; ok {
			values[i] = value
			continue
		}
		if pending, ok := cache.pending[key]; ok {
			waiting[key] = pending
			continue
		}
		pending := &pendingRead{done: make(chan struct{})}
		cache.pending[key] = pending
		waiting[key] = pending
		owned = append(owned, key)
		produced[key] = true
	}
	cache.mu.Unlock()
	if len(owned) > 0 {
		results, err := fetch(ctx, owned)
		if err == nil && len(results) != len(owned) {
			err = fmt.Errorf("immutable batch returned %d results for %d reads", len(results), len(owned))
		}
		cache.mu.Lock()
		for i, key := range owned {
			pending := waiting[key]
			pending.err = err
			if err == nil {
				pending.value, pending.err = results[i].Value, results[i].Err
			}
			delete(cache.pending, key)
			size := len(key) + len(pending.value)
			if pending.err == nil && len(cache.values) < 4096 && size <= (8<<20)-cache.bytes {
				cache.values[key] = pending.value
				cache.bytes += size
			}
			close(pending.done)
		}
		cache.mu.Unlock()
	}
	var joined error
	var retryKeys []string
	var retryIndices []int
	for i, key := range keys {
		pending, ok := waiting[key]
		if !ok {
			continue
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-pending.done:
		}
		if !produced[key] && (errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded)) && ctx.Err() == nil {
			retryKeys = append(retryKeys, key)
			retryIndices = append(retryIndices, i)
			continue
		}
		values[i] = pending.value
		if pending.err != nil && !errors.Is(joined, pending.err) {
			joined = errors.Join(joined, pending.err)
		}
	}
	if len(retryKeys) > 0 {
		retried, err := cache.LoadBatch(ctx, retryKeys, fetch)
		joined = errors.Join(joined, err)
		for i, index := range retryIndices {
			if i < len(retried) {
				values[index] = retried[i]
			}
		}
	}
	return values, joined
}
