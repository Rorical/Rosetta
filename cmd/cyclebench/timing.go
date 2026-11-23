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
)

// timedQuoter records the wall-clock duration of every quote it forwards,
// including failed ones, so quote latency can be reported apart from search
// overhead.
type timedQuoter struct {
	inner   arb.Quoter
	mu      sync.Mutex
	samples []time.Duration
}

// QuoteExactIn forwards one leg quote and records its duration. The mutex keeps
// the sample list safe under the engine's documented concurrent-use contract.
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

// writeTimingLine prints one stage's sample count and nearest-rank p50 and p99.
func writeTimingLine(w io.Writer, name string, samples []time.Duration) {
	fmt.Fprintf(w, "%s n=%d p50=%s p99=%s\n",
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
