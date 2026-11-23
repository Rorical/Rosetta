package main

import (
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/telemetry"
)

func TestWatchdogFiresOnlyAfterTheLoopStopsBeating(t *testing.T) {
	beat := telemetry.NewHeartbeat()
	now := time.Unix(1000, 0)
	limit := 90 * time.Second
	if _, stuck := stalled(beat, now, limit); stuck {
		t.Fatal("fired before the loop started")
	}
	beat.Beat(now)
	if _, stuck := stalled(beat, now.Add(limit), limit); stuck {
		t.Fatal("fired at the limit")
	}
	if age, stuck := stalled(beat, now.Add(limit+time.Second), limit); !stuck || age != limit+time.Second {
		t.Fatal("did not fire past the limit", age)
	}
}

func TestWatchdogLimitFromEnvironment(t *testing.T) {
	for text, want := range map[string]time.Duration{"": defaultWatchdog, "0": 0, "45s": 45 * time.Second} {
		t.Setenv("ROSETTA_WATCHDOG", text)
		if got, err := watchdogLimit(); err != nil || got != want {
			t.Fatal(text, got, err)
		}
	}
	t.Setenv("ROSETTA_WATCHDOG", "soon")
	if _, err := watchdogLimit(); err == nil {
		t.Fatal("accepted a malformed limit")
	}
}
