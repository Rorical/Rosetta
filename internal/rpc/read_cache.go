package rpc

import (
	"context"
	"errors"
	"sync"
)

// ReadCache owns bounded immutable read results. The owner binds its lifetime
// to one chain/snapshot or acquisition; keys must include state and overrides.
type ReadCache struct {
	mu      sync.Mutex
	values  map[string]string
	pending map[string]*pendingRead
	bytes   int
}
type pendingRead struct {
	done  chan struct{}
	value string
	err   error
}

// Load coalesces concurrent reads and caches successful results. Errors are not retained.
func (cache *ReadCache) Load(ctx context.Context, key string, fetch func(context.Context) (string, error)) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	cache.mu.Lock()
	if cache.values == nil {
		cache.values = make(map[string]string)
		cache.pending = make(map[string]*pendingRead)
	}
	cache.mu.Unlock()
	for {
		cache.mu.Lock()
		if value, ok := cache.values[key]; ok {
			cache.mu.Unlock()
			return value, nil
		}
		if pending, ok := cache.pending[key]; ok {
			cache.mu.Unlock()
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-pending.done:
			}
			// Another search may have owned the canceled request. An active waiter
			// can retry with its own context; failures are never cached.
			if ctx.Err() == nil && (errors.Is(pending.err, context.Canceled) || errors.Is(pending.err, context.DeadlineExceeded)) {
				continue
			}
			return pending.value, pending.err
		}
		pending := &pendingRead{done: make(chan struct{})}
		cache.pending[key] = pending
		cache.mu.Unlock()
		value, err := fetch(ctx)
		pending.value = value
		cache.mu.Lock()
		pending.err = err
		delete(cache.pending, key)
		size := len(key) + len(pending.value)
		if err == nil && len(cache.values) < 4096 && size <= (8<<20)-cache.bytes {
			cache.values[key] = pending.value
			cache.bytes += size
		}
		close(pending.done)
		cache.mu.Unlock()
		return pending.value, err
	}
}
