package remote

import (
	"math"
	"math/big"
	"slices"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// The sums a decision reads are kept by adding each window once and removing
// it on expiry or a settled outcome. After a day of that, past a rebase, they
// must describe the same model as sums rebuilt from the windows still held.
func TestWindowStatsMatchARebuild(t *testing.T) {
	h := newBidHistory()
	began := h.at
	rivals := []domain.Address{rival2905, rivalD4AF, rivalA408, predator}
	sets := [][]domain.PoolKey{route, route[:1], elsewhere, unrelated, {route[1], elsewhere[0]}}
	for i := range 26 * 60 {
		w := bidWindow{pools: sets[i%len(sets)], start: h.at, last: h.at, worth: uint64(20_000 * (1 + i%97)), first: i%11 == 0}
		if i%7 == 0 {
			w.worth = 0
		}
		for j := range i % 3 {
			w.contend(rivals[(i+j)%len(rivals)], domain.Address{}, uint64(500*(1+i%301)))
		}
		h.e.addWindow(w)
		h.at = h.at.Add(time.Minute)
	}
	// An outcome settled after its window closed rewrites it.
	start := h.at
	h.e.register(route, 0, start)
	h.e.mu.Lock()
	window := h.e.price(route, 1_000_000, start.Add(5*time.Millisecond))
	h.e.mu.Unlock()
	h.e.observe(rivalSighting{pools: route, from: rivalD4AF, tip: 400_000, block: 100, frame: 3}, start.Add(200*time.Millisecond))
	h.e.mu.Lock()
	h.e.expire(start.Add(3 * time.Second))
	h.e.mu.Unlock()
	h.e.observe(rivalSighting{pools: route, from: rivalA408, tip: 900_000, block: 100, frame: 3}, start.Add(4*time.Second))
	decision := &execution.BidDecision{ModelTip: big.NewInt(300_000), Win: 0.6, WinRaw: 0.6, Applied: true}
	h.e.settle(bidOutcomeOf(settlement(window, false, decision), start.Add(5*time.Second)))
	now := start.Add(5*time.Second + bidOutcomeDelay + time.Second)

	fresh := newBidEstimator(250*time.Millisecond, 550*time.Millisecond)
	h.e.mu.Lock()
	defer h.e.mu.Unlock()
	h.e.expire(now)
	if w, closed := h.e.find(window, route); !closed || !slices.Contains(w.contest, bidContest{from: rivalA408, tip: 900_000}) {
		t.Fatalf("the outcome did not rewrite its closed window: %+v", w)
	}
	if h.e.stats.epoch.Sub(began) < rebaseAfter {
		t.Fatalf("no rebase: epoch %v, history began %v", h.e.stats.epoch, began)
	}
	for _, w := range h.e.windows {
		fresh.addWindow(w)
	}
	if len(h.e.stats.rivals) != len(fresh.stats.rivals) || len(h.e.stats.routes) != len(fresh.stats.routes) {
		t.Fatalf("kept %d rivals and %d pool sets, a rebuild has %d and %d", len(h.e.stats.rivals), len(h.e.stats.routes), len(fresh.stats.rivals), len(fresh.stats.routes))
	}
	for _, worth := range []uint64{50_000, 1_000_000, 20_000_000} {
		kept, rebuilt := h.e.model(now, route, worth), fresh.model(now, route, worth)
		near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9*max(1, math.Abs(b)) }
		if !near(kept.win.First, rebuilt.win.First) || !near(kept.unknown, rebuilt.unknown) || len(kept.win.Contenders) != len(rebuilt.win.Contenders) {
			t.Fatalf("worth %d: first %v/%v unknown %v/%v contenders %d/%d", worth, kept.win.First, rebuilt.win.First, kept.unknown, rebuilt.unknown, len(kept.win.Contenders), len(rebuilt.win.Contenders))
		}
		for i, c := range kept.win.Contenders {
			if r := rebuilt.win.Contenders[i]; c.Name != r.Name || !near(c.Presence, r.Presence) {
				t.Fatalf("worth %d contender %d: %s %v, rebuilt %s %v", worth, i, c.Name, c.Presence, r.Name, r.Presence)
			}
		}
		for _, share := range []float64{0.01, 0.1, 0.3, 0.6, 1} {
			tip := uint64(float64(worth) * share)
			if a, b := kept.win.Raw(tip), rebuilt.win.Raw(tip); !near(a, b) {
				t.Fatalf("worth %d tip %d: P(win) %v, rebuilt %v", worth, tip, a, b)
			}
		}
	}
}
