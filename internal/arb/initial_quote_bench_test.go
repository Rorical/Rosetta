package arb

import (
	"context"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func BenchmarkRecordedFirstQuote(b *testing.B) {
	for _, mode := range []string{"optimized_only", "initial", "initial_diverse"} {
		b.Run(mode, func(b *testing.B) {
			engine, view, dirty, opts := recordedSearchFixture(b)
			opts.StreamInitial = mode != "optimized_only"
			opts.Diverse = mode == "initial_diverse"
			ctx, cancel := context.WithCancel(b.Context())
			w, err := NewWorker(ctx, engine, opts)
			if err != nil {
				b.Fatal(err)
			}
			defer func() { cancel(); <-w.Done() }()
			var firstTime, allInitialTime, lastInitialTime time.Duration
			var initials uint64
			iteration := uint64(0)
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				iteration++
				started := time.Now()
				job := SearchJob{Incremental: true, View: view, Dirty: dirty, GraphVersion: 1, Version: domain.StateVersion{Source: domain.SourceFast, Epoch: iteration, BlockNumber: iteration}, ReceivedAt: started}
				if err := w.Submit(job); err != nil {
					b.Fatal(err)
				}
				first := true
				var lastInitial time.Duration
				for {
					event := <-w.Results()
					if event.Err != nil || !w.Accept(event) {
						releaseSearch(event.Job)
						b.Fatal("quote benchmark", event.Err)
					}
					if event.Stage == QuoteInitial {
						initials++
						elapsed := event.QuotedAt.Sub(started)
						allInitialTime += elapsed
						lastInitial = max(lastInitial, elapsed)
					}
					if first && event.Partial {
						firstTime += event.QuotedAt.Sub(started)
						first = false
					}
					releaseSearch(event.Job)
					if !event.Partial {
						lastInitialTime += lastInitial
						break
					}
				}
			}
			b.ReportMetric(float64(firstTime.Microseconds())/float64(b.N), "first-us/op")
			if initials > 0 {
				b.ReportMetric(float64(allInitialTime.Microseconds())/float64(initials), "initial-mean-us/quote")
				b.ReportMetric(float64(lastInitialTime.Microseconds())/float64(b.N), "last-initial-us/op")
				b.ReportMetric(float64(initials)/float64(b.N), "initials/op")
			}
		})
	}
}
