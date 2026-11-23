package graph

import (
	"context"
	"errors"
	"testing"
	"time"
)

type enumerationContext struct {
	context.Context
	checks           int
	entered, release chan struct{}
}

func (c *enumerationContext) Err() error {
	c.checks++
	if c.checks == 5 {
		close(c.entered)
		<-c.release
	}
	return c.Context.Err()
}

func TestBackgroundEnumerationCancelsDuringTraversal(t *testing.T) {
	graph := mustBuild(t, testPool(1, 1, 2), testPool(2, 2, 3), testPool(3, 3, 1), testPool(4, 1, 3))
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	observed := &enumerationContext{Context: ctx, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		table, err := BuildTableContext(observed, graph, 4, 1000)
		if table != nil {
			done <- errors.New("canceled graph published a partial table")
			return
		}
		done <- err
	}()
	select {
	case <-observed.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("enumeration never reached its inner cancellation check")
	}
	cancel()
	close(observed.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("traversal cancellation: %v", err)
	}
}
