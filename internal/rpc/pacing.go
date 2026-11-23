package rpc

import (
	"context"
	"time"
)

// Participating clients share pacing across bootstrap, polling and search.
// The gate is released before I/O; a slow response does not serialize other reads.
func (c *RequestPolicy) pace(ctx context.Context, calls uint64) error {
	select {
	case c.pacing <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.pacing }()
	for {
		deadline := max(c.nextRequest.UnixNano(), c.throttledUntil.Load())
		if err := wait(ctx, time.Until(time.Unix(0, deadline))); err != nil {
			return err
		}
		if c.throttledUntil.Load() <= time.Now().UnixNano() {
			break
		}
	}
	c.nextRequest = time.Now().Add(c.minInterval * time.Duration(max(uint64(1), calls)))
	return nil
}
