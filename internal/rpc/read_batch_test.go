package rpc

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func TestBatchReadCacheKeepsSuccessfulMembersOnPartialFailure(t *testing.T) {
	cache := &ReadCache{}
	bad := errors.New("limited")
	fetches := 0
	fetch := func(_ context.Context, keys []string) ([]ReadResult, error) {
		fetches++
		if fetches == 1 {
			if !reflect.DeepEqual(keys, []string{"a", "b"}) {
				t.Fatal(keys)
			}
			return []ReadResult{{Value: "first"}, {Err: bad}}, nil
		}
		if !reflect.DeepEqual(keys, []string{"b"}) {
			t.Fatal("successful or duplicate request repeated", keys)
		}
		return []ReadResult{{Value: "second"}}, nil
	}
	if _, err := cache.LoadBatch(t.Context(), []string{"a", "b", "a"}, fetch); !errors.Is(err, bad) {
		t.Fatal(err)
	}
	values, err := cache.LoadBatch(t.Context(), []string{"a", "b", "a"}, fetch)
	if err != nil || !reflect.DeepEqual(values, []string{"first", "second", "first"}) {
		t.Fatal(values, err)
	}
	if _, err = cache.Load(t.Context(), "b", func(context.Context) (string, error) {
		t.Fatal("single read did not share batch cache")
		return "", nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestBatchWaiterSurvivesCanceledSingleProducer(t *testing.T) {
	cache := &ReadCache{}
	started := make(chan struct{})
	ctx, cancel := context.WithCancel(t.Context())
	producer := make(chan error, 1)
	go func() {
		_, err := cache.Load(ctx, "a", func(ctx context.Context) (string, error) { close(started); <-ctx.Done(); return "", ctx.Err() })
		producer <- err
	}()
	<-started
	waiter := make(chan error, 1)
	waitContext := &batchWaitContext{Context: t.Context(), entered: make(chan struct{})}
	go func() {
		values, err := cache.LoadBatch(waitContext, []string{"a", "a"}, func(_ context.Context, keys []string) ([]ReadResult, error) {
			if !reflect.DeepEqual(keys, []string{"a"}) {
				t.Error(keys)
			}
			return []ReadResult{{Value: "recovered"}}, nil
		})
		if err == nil && !reflect.DeepEqual(values, []string{"recovered", "recovered"}) {
			err = errors.New("active batch lost its values")
		}
		waiter <- err
	}()
	<-waitContext.entered
	cancel()
	<-producer
	if err := <-waiter; err != nil {
		t.Fatal(err)
	}
}

type batchWaitContext struct {
	context.Context
	entered chan struct{}
	once    sync.Once
}

func (c *batchWaitContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	return c.Context.Done()
}

func TestFailedBatchDoesNotRepeatOneErrorForEveryPendingRead(t *testing.T) {
	cache := &ReadCache{}
	keys := make([]string, 1024)
	for i := range keys {
		keys[i] = fmt.Sprint(i)
	}
	_, err := cache.LoadBatch(t.Context(), keys, func(_ context.Context, missing []string) ([]ReadResult, error) { return nil, context.DeadlineExceeded })
	if !errors.Is(err, context.DeadlineExceeded) || len(err.Error()) > 100 {
		t.Fatal("one acquisition failure produced an oversized aggregate")
	}
}
