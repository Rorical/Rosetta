package main

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// fixedQuoter is an arb.Quoter that returns a fixed amount without touching
// state, so timing tests stay offline and deterministic.
type fixedQuoter struct {
	out   *big.Int
	err   error
	calls int
}

func (q *fixedQuoter) QuoteExactIn(context.Context, pool.State, pool.DeepState, domain.Address, *big.Int) (*big.Int, error) {
	q.calls++
	if q.err != nil {
		return nil, q.err
	}
	return new(big.Int).Set(q.out), nil
}

func TestPercentile(t *testing.T) {
	micro := time.Microsecond
	// sequence(n) holds 1us..nus; its nearest-rank percentiles are easy to
	// verify by hand.
	sequence := func(n int) []time.Duration {
		samples := make([]time.Duration, n)
		for i := range samples {
			samples[i] = time.Duration(i+1) * micro
		}
		return samples
	}

	tests := []struct {
		name    string
		samples []time.Duration
		p       float64
		want    time.Duration
	}{
		{name: "nil", samples: nil, p: 0.5, want: 0},
		{name: "empty", samples: []time.Duration{}, p: 0.99, want: 0},
		{name: "one sample p50", samples: sequence(1), p: 0.50, want: micro},
		{name: "one sample p99", samples: sequence(1), p: 0.99, want: micro},
		{name: "ten samples p50", samples: sequence(10), p: 0.50, want: 5 * micro},
		{name: "ten samples p99", samples: sequence(10), p: 0.99, want: 10 * micro},
		{name: "hundred samples p50", samples: sequence(100), p: 0.50, want: 50 * micro},
		{name: "hundred samples p99", samples: sequence(100), p: 0.99, want: 99 * micro},
		{name: "p zero clamps to smallest", samples: sequence(3), p: 0, want: micro},
		{name: "p above one clamps to largest", samples: sequence(3), p: 1.5, want: 3 * micro},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := percentile(test.samples, test.p); got != test.want {
				t.Fatalf("percentile(%d samples, %v) = %s, want %s",
					len(test.samples), test.p, got, test.want)
			}
		})
	}
}

func TestPercentileLeavesSamplesInOrder(t *testing.T) {
	samples := []time.Duration{30, 10, 20}
	before := slices.Clone(samples)

	percentile(samples, 0.5)

	if !slices.Equal(samples, before) {
		t.Fatalf("percentile reordered its input: %v, want %v", samples, before)
	}
}

func TestTimedQuoterRecordsEveryCall(t *testing.T) {
	inner := &fixedQuoter{out: big.NewInt(42)}
	quoter := &timedQuoter{inner: inner}
	ctx := context.Background()

	for i := 0; i < 3; i++ {
		amountOut, err := quoter.QuoteExactIn(ctx, pool.State{}, pool.DeepState{}, syntheticAddress(tokenAddressClass, 0, 0), big.NewInt(7))
		if err != nil {
			t.Fatalf("call %d: %v", i, err)
		}
		if amountOut.String() != "42" {
			t.Fatalf("call %d amount out = %s, want 42", i, amountOut)
		}
	}
	if inner.calls != 3 {
		t.Fatalf("inner calls = %d, want 3", inner.calls)
	}

	samples := quoter.snapshot()
	if len(samples) != 3 {
		t.Fatalf("sample count = %d, want 3", len(samples))
	}
	for i, sample := range samples {
		if sample < 0 {
			t.Errorf("sample %d = %s, want non-negative", i, sample)
		}
	}

	// A snapshot must not alias the live slice: clobbering it cannot leak into
	// the next snapshot or the reset count.
	samples[0] = time.Hour
	if got := quoter.snapshot(); len(got) != 3 || got[0] == time.Hour {
		t.Fatalf("snapshot aliases the sample slice: %v", got)
	}

	quoter.reset()
	if got := quoter.snapshot(); len(got) != 0 {
		t.Fatalf("samples after reset = %d, want 0", len(got))
	}
}

func TestTimedQuoterRecordsFailedQuotes(t *testing.T) {
	quoter := &timedQuoter{inner: &fixedQuoter{err: errors.New("quote failed")}}

	if _, err := quoter.QuoteExactIn(context.Background(), pool.State{}, pool.DeepState{}, syntheticAddress(tokenAddressClass, 0, 0), big.NewInt(7)); err == nil {
		t.Fatal("QuoteExactIn succeeded, want error")
	}
	if got := len(quoter.snapshot()); got != 1 {
		t.Fatalf("sample count after error = %d, want 1", got)
	}
}

func TestWriteTimingLine(t *testing.T) {
	var out strings.Builder
	writeTimingLine(&out, "search", []time.Duration{123_400 * time.Nanosecond, 456_700 * time.Nanosecond})

	want := "search n=2 p50=123.4us p99=456.7us\n"
	if got := out.String(); got != want {
		t.Fatalf("timing line = %q, want %q", got, want)
	}
}
