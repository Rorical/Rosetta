package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestHeartbeatReportsAgeOnlyAfterTheFirstBeat(t *testing.T) {
	h := NewHeartbeat()
	now := time.Unix(1000, 0)
	if _, ok := h.Since(now); ok {
		t.Fatal("age reported before any beat")
	}
	h.Beat(now)
	if age, ok := h.Since(now.Add(3 * time.Second)); !ok || age != 3*time.Second {
		t.Fatal("age", age, ok)
	}
	if HeartbeatFrom(WithHeartbeat(context.Background(), h)) != h || HeartbeatFrom(context.Background()) != nil {
		t.Fatal("context round trip")
	}
	var none *Heartbeat
	none.Beat(now) // a loop without a watchdog beats into nil
}
