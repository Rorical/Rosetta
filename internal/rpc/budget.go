package rpc

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// ErrCallBudgetExceeded means an attempt was rejected before provider I/O.
var ErrCallBudgetExceeded = errors.New("RPC method-call budget exhausted")

// RequestObservation counts method invocations, including each batch member.
// Beacon REST reads each count as one unit. Providers may charge different compute units; these counts are not prices.
type RequestObservation struct {
	Calls  uint64
	Retry  bool
	Caller Caller // from the request context; CallerOther when unset
}

// BatchMethod names a JSON-RPC batch, whose members may differ.
const BatchMethod = "batch"

// ResponseObservation describes one finished HTTP attempt.
type ResponseObservation struct {
	Method    string
	Elapsed   time.Duration
	Failed    bool
	Throttled bool
}

func (c *RequestPolicy) reserveCalls(ctx context.Context, amount uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	for {
		used := c.calls.Load()
		if c.maxCalls > 0 && (used >= c.maxCalls || amount > c.maxCalls-used) {
			return fmt.Errorf("%w: limit %d", ErrCallBudgetExceeded, c.maxCalls)
		}
		if c.calls.CompareAndSwap(used, used+amount) {
			return nil
		}
	}
}
