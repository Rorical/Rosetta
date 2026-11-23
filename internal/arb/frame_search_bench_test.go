package arb

import (
	"context"
	"syscall"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// One streamed search per frame with the live worker options. "full" marks
// every pool dirty in a new block; "incremental" keeps one block and moves the
// reserve-based pools each frame, so ranking and exact results are reused
// exactly as consecutive flashblocks reuse them. "exact4" sets ExactK to 4.
func BenchmarkFrameSearch(b *testing.B) {
	for _, mode := range []struct {
		name        string
		incremental bool
		exactK      int
	}{{"full", false, 0}, {"incremental", true, 0}, {"incremental-exact4", true, 4}} {
		b.Run(mode.name, func(b *testing.B) {
			fixture := newFrameFixture(b)
			fixture.opts.ExactK = mode.exactK
			ctx, cancel := context.WithCancel(b.Context())
			w, err := NewWorker(ctx, fixture.engine, fixture.opts)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { cancel(); <-w.Done() }()
			var first time.Duration
			var optimized, initial, selected, probeOnly int
			frame := uint64(0)
			search := func() {
				frame++
				job := SearchJob{Incremental: true, View: fixture.base, Dirty: fixture.all, GraphVersion: 1, ReceivedAt: time.Now(),
					Version: domain.StateVersion{Source: domain.SourceFast, Epoch: 1, BlockNumber: frame}}
				if mode.incremental {
					job.Version.BlockNumber, job.Version.TxIndex = 1, frame
					job.Dirty = fixture.reserveSet
					if frame%2 == 0 {
						job.View = fixture.changed
					}
				}
				if err := w.Submit(job); err != nil {
					b.Fatal(err)
				}
				seen := false
				for {
					event := <-w.Results()
					if event.Err != nil || !w.Rebase(&event) || !w.Accept(event) {
						b.Fatal("frame search", event.Err)
					}
					releaseSearch(event.Job)
					switch {
					case !event.Partial:
						selected += event.Result.Selected
						probeOnly += event.Result.ProbeOnly
						return
					case event.Stage == QuoteInitial:
						initial++
					default:
						optimized++
						if !seen {
							seen = true
							first += event.QuotedAt.Sub(job.ReceivedAt)
						}
					}
				}
			}
			search() // The first frame fills the lane caches in incremental mode.
			first, optimized, initial, selected, probeOnly = 0, 0, 0, 0, 0
			b.ReportAllocs()
			b.ResetTimer()
			started := processCPU(b)
			for b.Loop() {
				search()
			}
			b.ReportMetric(float64((processCPU(b)-started).Microseconds())/float64(b.N), "cpu-us/op")
			b.ReportMetric(float64(first.Microseconds())/float64(b.N), "first-opt-us/op")
			b.ReportMetric(float64(optimized)/float64(b.N), "optimized/op")
			b.ReportMetric(float64(initial)/float64(b.N), "initial/op")
			b.ReportMetric(float64(selected-probeOnly)/float64(b.N), "sized/op")
			b.ReportMetric(float64(probeOnly)/float64(b.N), "probe-only/op")
		})
	}
}

// Coarse ranking alone for one frame: the compiled table with its lane cache,
// and the generic path that ranks supplemental catalog cycles.
func BenchmarkFrameRank(b *testing.B) {
	for _, mode := range []string{"compiled", "generic"} {
		b.Run(mode, func(b *testing.B) {
			fixture := newFrameFixture(b)
			var cache rankCache
			cache.bind(fixture.engine.compiled, 1)
			var cycles []graph.Cycle
			for index := range fixture.engine.table.Len() {
				cycles = append(cycles, fixture.engine.table.Cycle(index))
			}
			frame := 0
			rank := func() {
				view := fixture.base
				if frame++; frame%2 == 0 {
					view = fixture.changed
				}
				var err error
				if mode == "compiled" {
					_, err = fixture.engine.rankWithWorkers(b.Context(), view, fixture.reserveSet, fixture.opts, &cache, 1)
				} else {
					_, err = fixture.engine.rankExplored(b.Context(), view, cycles, fixture.reserveSet, fixture.opts, 1)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
			rank()
			b.ReportAllocs()
			b.ResetTimer()
			started := processCPU(b)
			for b.Loop() {
				rank()
			}
			b.ReportMetric(float64((processCPU(b)-started).Microseconds())/float64(b.N), "cpu-us/op")
		})
	}
}

// processCPU is the process's user and system time. Wall time alone is noisy
// on a shared host; CPU per frame is what the two-core production box spends.
func processCPU(b *testing.B) time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		b.Fatal(err)
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}
