package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"runtime/pprof"
	"time"

	"github.com/Rorical/Rosetta/internal/telemetry"
)

// watchdogExit is the exit status after a main-loop stall; systemd restarts
// the unit on it.
const watchdogExit = 75

// defaultWatchdog is how long a main-loop handler may run before the process
// exits. Rebuilding the topology on the loop takes seconds, so the limit is
// well above that; ROSETTA_WATCHDOG overrides it and 0 disables it.
const defaultWatchdog = 90 * time.Second

func watchdogLimit() (time.Duration, error) {
	text := os.Getenv("ROSETTA_WATCHDOG")
	if text == "" {
		return defaultWatchdog, nil
	}
	if text == "0" {
		return 0, nil
	}
	limit, err := time.ParseDuration(text)
	if err != nil || limit < 0 {
		return 0, fmt.Errorf("ROSETTA_WATCHDOG %q: want a duration such as 90s, or 0", text)
	}
	return limit, nil
}

// stalled reports whether the main loop has not beaten for longer than limit.
func stalled(beat *telemetry.Heartbeat, now time.Time, limit time.Duration) (time.Duration, bool) {
	age, ok := beat.Since(now)
	return age, ok && age > limit
}

// watch exits the process when the main loop stops beating, after writing
// every goroutine's stack to the log. It stops when ctx ends, so a slow
// shutdown that persists state is not cut short.
func watch(ctx context.Context, beat *telemetry.Heartbeat, limit time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			age, stuck := stalled(beat, now, limit)
			if !stuck {
				continue
			}
			logger.Error("main loop stalled; exiting so the service restarts", "stalled", age.Round(time.Millisecond), "limit", limit)
			_ = pprof.Lookup("goroutine").WriteTo(os.Stderr, 2)
			os.Exit(watchdogExit)
		}
	}
}
