package rpc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestReadCacheCanceledProducerDoesNotPoisonActiveWaiter(t *testing.T) {
	cache := &ReadCache{}
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	producer := make(chan error, 1)
	go func() {
		_, err := cache.Load(ctx, "hash/key", func(ctx context.Context) (string, error) {
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		})
		producer <- err
	}()
	<-started
	var fetched atomic.Int32
	waiter := make(chan error, 1)
	go func() {
		value, err := cache.Load(t.Context(), "hash/key", func(context.Context) (string, error) {
			fetched.Add(1)
			return "0x1234", nil
		})
		if value != "0x1234" && err == nil {
			err = errors.New("active waiter did not obtain its own result")
		}
		waiter <- err
	}()
	cancel()
	if err := <-producer; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load(t.Context(), "hash/key", func(context.Context) (string, error) {
		t.Fatal("successful retry was not cached")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
	if fetched.Load() != 1 {
		t.Fatal("waiter retried more than once")
	}
}
