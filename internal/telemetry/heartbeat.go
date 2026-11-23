package telemetry

import (
	"context"
	"sync/atomic"
	"time"
)

// Heartbeat records the main loop's progress for a watchdog outside it. A loop waiting in select still beats on its timer; only a handler
// that does not return stops the beats.
type Heartbeat struct{ at atomic.Int64 }

func NewHeartbeat() *Heartbeat { return &Heartbeat{} }

// Beat records progress at now. A nil Heartbeat ignores it.
func (h *Heartbeat) Beat(now time.Time) {
	if h != nil {
		h.at.Store(now.UnixNano())
	}
}

// Since reports how long ago the last beat was; false before the first.
func (h *Heartbeat) Since(now time.Time) (time.Duration, bool) {
	at := h.at.Load()
	if at == 0 {
		return 0, false
	}
	return now.Sub(time.Unix(0, at)), true
}

type heartbeatKey struct{}

// WithHeartbeat hands h to the main loop that runs under ctx.
func WithHeartbeat(ctx context.Context, h *Heartbeat) context.Context {
	return context.WithValue(ctx, heartbeatKey{}, h)
}

// HeartbeatFrom returns the Heartbeat set by WithHeartbeat, or nil.
func HeartbeatFrom(ctx context.Context) *Heartbeat {
	h, _ := ctx.Value(heartbeatKey{}).(*Heartbeat)
	return h
}
