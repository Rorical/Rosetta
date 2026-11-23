package main

import (
	"context"
	"fmt"
	"io"
	"math"
	"math/big"
	"slices"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// timedQuoter records the wall-clock duration of every quote it forwards,
// including failed ones. It sits between the engine and the real quoter so
// quoting cost can be reported apart from search overhead.
type timedQuoter struct {
	inner   arb.Quoter
	mu      sync.Mutex
	samples []time.Duration
}

// QuoteExactIn forwards one leg quote and records how long it took. The mutex
// guards samples: the engine quotes serially today, but it documents itself as
// safe for concurrent use, so this wrapper stays safe under the same contract.
func (q *timedQuoter) QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	start := time.Now()
	amountOut, err := q.inner.QuoteExactIn(ctx, state, deep, tokenIn, amountIn)
	elapsed := time.Since(start)

	q.mu.Lock()
	q.samples = append(q.samples, elapsed)
	q.mu.Unlock()
	return amountOut, err
}

// reset discards the samples collected so far.
func (q *timedQuoter) reset() {
	q.mu.Lock()
	q.samples = q.samples[:0]
	q.mu.Unlock()
}

// snapshot returns a copy of the samples, so a later reset or append cannot
// change a report that is still being assembled.
func (q *timedQuoter) snapshot() []time.Duration {
	q.mu.Lock()
	defer q.mu.Unlock()
	return slices.Clone(q.samples)
}

// percentile returns the nearest-rank percentile of durations: the sample at
// index ceil(p*n)-1 of the sorted set, clamped to the valid range. An empty set
// has no percentile and reports zero.
func percentile(durations []time.Duration, p float64) time.Duration {
	if len(durations) == 0 {
		return 0
	}
	sorted := slices.Clone(durations)
	slices.Sort(sorted)
	index := int(math.Ceil(p*float64(len(sorted)))) - 1
	index = max(0, min(index, len(sorted)-1))
	return sorted[index]
}

// timingReport holds one sample set per measured stage.
type timingReport struct {
	Search      []time.Duration
	EngineQuote []time.Duration
	Direct      []time.Duration
	DirectQuote []time.Duration
}

// writeTo prints one line per stage with nearest-rank p50 and p99.
func (r timingReport) writeTo(w io.Writer) {
	writeTimingLine(w, "search", r.Search)
	writeTimingLine(w, "engine_quote", r.EngineQuote)
	writeTimingLine(w, "direct", r.Direct)
	writeTimingLine(w, "direct_quote", r.DirectQuote)
}

func writeTimingLine(w io.Writer, name string, samples []time.Duration) {
	fmt.Fprintf(w, "timing %s n=%d p50=%s p99=%s\n",
		name,
		len(samples),
		formatMicros(percentile(samples, 0.50)),
		formatMicros(percentile(samples, 0.99)),
	)
}

// formatMicros renders a duration in microseconds with one decimal. ASCII "us"
// keeps the line copyable through terminals that mangle the micro sign.
func formatMicros(d time.Duration) string {
	return fmt.Sprintf("%.1fus", float64(d)/float64(time.Microsecond))
}

// measurement is one run's kept results and timing samples.
type measurement struct {
	Opportunities []arb.Opportunity
	Direct        directQuote
	Timing        timingReport
}

// measure runs the configured number of evaluations and direct quotes against
// read-only state and collects their durations. It is the only place the
// config's counts are consumed, so a reversed assignment is caught by
// TestMeasureUsesConfigCountsInOrder.
func measure(ctx context.Context, cfg config, engine *arb.Engine, store *state.Store, dirty []domain.PoolKey, onchain onChainData, timed *timedQuoter) (measurement, error) {
	warmup, repeat := cfg.measurements()

	opportunities, searchSamples, err := evaluateRepeatedly(ctx, engine, store, dirty, warmup, repeat, timed)
	if err != nil {
		return measurement{}, fmt.Errorf("evaluate: %w", err)
	}
	engineQuoteSamples := timed.snapshot()

	direct, directSamples, err := quoteRepeatedly(ctx, timed, store, onchain, warmup, repeat)
	if err != nil {
		return measurement{}, err
	}
	directQuoteSamples := timed.snapshot()

	return measurement{
		Opportunities: opportunities,
		Direct:        direct,
		Timing: timingReport{
			Search:      searchSamples,
			EngineQuote: engineQuoteSamples,
			Direct:      directSamples,
			DirectQuote: directQuoteSamples,
		},
	}, nil
}

// evaluateRepeatedly warms up, resetting the quote samples afterwards so warmup
// quotes stay out of the report, then measures repeat evaluations. It returns
// the first measured result and one search duration per measured call.
//
// Evaluate is read-only and the engine deterministic, so every call sees the
// same state and yields the same ranking; discarding warmup results changes
// nothing about the report.
func evaluateRepeatedly(ctx context.Context, engine *arb.Engine, view arb.View, dirty []domain.PoolKey, warmup, repeat int, timed *timedQuoter) ([]arb.Opportunity, []time.Duration, error) {
	for i := 0; i < warmup; i++ {
		if _, err := engine.Evaluate(ctx, view, dirty); err != nil {
			return nil, nil, err
		}
	}
	timed.reset()

	var kept []arb.Opportunity
	durations := make([]time.Duration, 0, repeat)
	for i := 0; i < repeat; i++ {
		start := time.Now()
		opportunities, err := engine.Evaluate(ctx, view, dirty)
		durations = append(durations, time.Since(start))
		if err != nil {
			return nil, nil, err
		}
		if i == 0 {
			kept = opportunities
		}
	}
	return kept, durations, nil
}

// quoteRepeatedly mirrors evaluateRepeatedly for the direct on-chain path. The
// returned quote is the first measured one; every repeat recomputes it from the
// same read-only state.
func quoteRepeatedly(ctx context.Context, timed *timedQuoter, store *state.Store, onchain onChainData, warmup, repeat int) (directQuote, []time.Duration, error) {
	for i := 0; i < warmup; i++ {
		if _, err := quoteOnChain(ctx, timed, store, onchain); err != nil {
			return directQuote{}, nil, err
		}
	}
	timed.reset()

	var kept directQuote
	durations := make([]time.Duration, 0, repeat)
	for i := 0; i < repeat; i++ {
		start := time.Now()
		quote, err := quoteOnChain(ctx, timed, store, onchain)
		durations = append(durations, time.Since(start))
		if err != nil {
			return directQuote{}, nil, err
		}
		if i == 0 {
			kept = quote
		}
	}
	return kept, durations, nil
}
