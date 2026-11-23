package remote

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// capturedWindows loads canonical OP pools whose windows the repair loop had
// already widened: the wstETH/WETH Slipstream pool at the 64-word cap and a
// thin V4 pool (spacing 1, tick 0) holding five words.
func capturedWindows(t *testing.T) (*runtime, domain.PoolKey, domain.PoolKey) {
	t.Helper()
	raw, err := os.ReadFile("../math/testdata/op-windows-158960149.json")
	if err != nil {
		t.Fatal(err)
	}
	store := state.New()
	if err := store.LoadSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	var slipstream, v4 domain.PoolKey
	for _, p := range store.Pools() {
		if p.Key.Kind == domain.PoolKindV4 {
			v4 = p.Key
		} else {
			slipstream = p.Key
		}
	}
	r := &runtime{store: store, policy: pipeline.SearchPolicy{WordRadius: 1}}
	r.ticks = &tickRepair{jobs: make(chan tickTask, 1), demand: map[domain.PoolKey]int16{}, retryAt: map[domain.PoolKey]uint64{}}
	return r, slipstream, v4
}

func outOfWindow(key domain.PoolKey) arb.QuoteIssue {
	return arb.QuoteIssue{Pool: key, Err: fmt.Errorf("quote: %w: final tick 168808 outside [-512, 767]", math.ErrOutOfWindow)}
}

// The old loop doubled from the current width (5 words -> radius 4) and then
// kept doubling to 64 words. Requests now load the coverage radius directly.
func TestWindowExitRequestsCoverageRadius(t *testing.T) {
	r, _, v4 := capturedWindows(t)
	r.ticks.request(r, outOfWindow(v4))
	if got := r.ticks.demand[v4]; got != pool.WindowRadius(1, 1) {
		t.Fatalf("narrow V4 window repair radius %d, want %d", got, pool.WindowRadius(1, 1))
	}
}

// A window that already covers the target is not reloaded by a late issue from
// an older view; the capped 129-word pool used to request nothing and stay
// failing, now its exits are capacity and never reach the repair queue.
func TestCoveredWindowIgnoresStaleExit(t *testing.T) {
	r, slipstream, _ := capturedWindows(t)
	r.ticks.request(r, outOfWindow(slipstream))
	if _, queued := r.ticks.demand[slipstream]; queued {
		t.Fatal("covered window queued again")
	}
	r.ticks.request(r, arb.QuoteIssue{Pool: slipstream, Err: arb.ErrMissingDeep})
	if got := r.ticks.demand[slipstream]; got != pool.WindowRadius(1, 1) {
		t.Fatalf("stale ticks reload radius %d", got)
	}
}

// Schedule upgrades a window narrower than half the coverage target before any
// quote fails, and loads the coverage radius rather than the configured floor.
func TestScheduleUpgradesNarrowWindow(t *testing.T) {
	r, slipstream, v4 := capturedWindows(t)
	r.ticks.schedule(t.Context(), r)
	select {
	case task := <-r.ticks.jobs:
		task.reader.Close()
		if task.state.Key != v4 || task.radius != pool.WindowRadius(1, 1) {
			t.Fatalf("scheduled %s radius %d", strings.ToLower(task.state.Key.PoolID.Hex()), task.radius)
		}
	default:
		t.Fatal("narrow window was not scheduled")
	}
	if _, queued := r.ticks.demand[slipstream]; queued {
		t.Fatal("covered window scheduled")
	}
}
