package main

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
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
		{name: "two samples p50", samples: sequence(2), p: 0.50, want: micro},
		{name: "two samples p99", samples: sequence(2), p: 0.99, want: 2 * micro},
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
		amountOut, err := quoter.QuoteExactIn(ctx, pool.State{}, pool.DeepState{}, testAddress(0x01), big.NewInt(7))
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

	if _, err := quoter.QuoteExactIn(context.Background(), pool.State{}, pool.DeepState{}, testAddress(0x01), big.NewInt(7)); err == nil {
		t.Fatal("QuoteExactIn succeeded, want error")
	}
	if got := len(quoter.snapshot()); got != 1 {
		t.Fatalf("sample count after error = %d, want 1", got)
	}
}

func TestParseFlagsTiming(t *testing.T) {
	tests := []struct {
		name     string
		args     []string
		wantErr  string
		wantRep  int
		wantWarm int
		wantTime bool
	}{
		{
			name:     "defaults",
			args:     []string{"-case", "case.json"},
			wantRep:  10,
			wantWarm: 1,
			wantTime: true,
		},
		{
			name:     "explicit",
			args:     []string{"-case", "case.json", "-repeat", "20", "-warmup", "0", "-timing=false"},
			wantRep:  20,
			wantWarm: 0,
			wantTime: false,
		},
		{
			name:    "repeat zero",
			args:    []string{"-case", "case.json", "-repeat", "0"},
			wantErr: "-repeat 0",
		},
		{
			name:    "repeat negative",
			args:    []string{"-case", "case.json", "-repeat", "-1"},
			wantErr: "-repeat -1",
		},
		{
			name:    "warmup negative",
			args:    []string{"-case", "case.json", "-warmup", "-1"},
			wantErr: "-warmup -1",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseFlags(test.args)
			if test.wantErr != "" {
				if err == nil {
					t.Fatalf("parseFlags(%q) succeeded, want error", test.args)
				}
				if !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("error %q does not contain %q", err, test.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseFlags(%q): %v", test.args, err)
			}
			if cfg.repeat != test.wantRep {
				t.Errorf("repeat = %d, want %d", cfg.repeat, test.wantRep)
			}
			if cfg.warmup != test.wantWarm {
				t.Errorf("warmup = %d, want %d", cfg.warmup, test.wantWarm)
			}
			if cfg.timing != test.wantTime {
				t.Errorf("timing = %t, want %t", cfg.timing, test.wantTime)
			}
		})
	}
}

func TestConfigMeasurements(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantWarmup int
		wantRepeat int
	}{
		{
			name:       "flags map in order",
			args:       []string{"-case", "case.json", "-repeat", "7", "-warmup", "2"},
			wantWarmup: 2,
			wantRepeat: 7,
		},
		{
			name:       "zero warmup",
			args:       []string{"-case", "case.json", "-repeat", "4", "-warmup", "0"},
			wantWarmup: 0,
			wantRepeat: 4,
		},
		{
			name:       "timing off runs once",
			args:       []string{"-case", "case.json", "-repeat", "20", "-warmup", "5", "-timing=false"},
			wantWarmup: 0,
			wantRepeat: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseFlags(test.args)
			if err != nil {
				t.Fatalf("parseFlags(%q): %v", test.args, err)
			}
			warmup, repeat := cfg.measurements()
			if warmup != test.wantWarmup || repeat != test.wantRepeat {
				t.Fatalf("measurements = warmup %d repeat %d, want warmup %d repeat %d",
					warmup, repeat, test.wantWarmup, test.wantRepeat)
			}
		})
	}
}

func TestTimingLineFormat(t *testing.T) {
	var out strings.Builder
	timingReport{
		Search:      []time.Duration{123_400 * time.Nanosecond, 456_700 * time.Nanosecond},
		EngineQuote: []time.Duration{12_300 * time.Nanosecond},
		Direct:      []time.Duration{30_100 * time.Nanosecond},
		DirectQuote: nil,
	}.writeTo(&out)

	want := strings.Join([]string{
		"timing search n=2 p50=123.4us p99=456.7us",
		"timing engine_quote n=1 p50=12.3us p99=12.3us",
		"timing direct n=1 p50=30.1us p99=30.1us",
		"timing direct_quote n=0 p50=0.0us p99=0.0us",
	}, "\n") + "\n"
	if got := out.String(); got != want {
		t.Fatalf("timing report = %q, want %q", got, want)
	}
}

// stubTable serves one prepared cycle so Evaluate runs without a built graph.
type stubTable struct {
	cycle graph.Cycle
}

func (t stubTable) Len() int                               { return 1 }
func (t stubTable) Cycle(int) graph.Cycle                  { return t.cycle }
func (t stubTable) Candidates([]domain.PoolKey, int) []int { return []int{0} }

// measureFixture seeds three V2 pools in a cycle on a real store, so the engine
// and the direct path both run offline. The fixed quoter ignores pricing state,
// so the pools need only their token pair.
func measureFixture(t *testing.T) (*state.Store, []domain.PoolKey, graph.Cycle, onChainData) {
	t.Helper()

	tokenA, tokenB, tokenC := testAddress(0x11), testAddress(0x12), testAddress(0x13)
	pAB := domain.PoolKey{Address: testAddress(0x21), Kind: domain.PoolKindV2}
	pBC := domain.PoolKey{Address: testAddress(0x22), Kind: domain.PoolKindV2}
	pCA := domain.PoolKey{Address: testAddress(0x23), Kind: domain.PoolKindV2}

	store := state.New()
	for _, shallow := range []pool.State{
		{Key: pAB, Token0: tokenA, Token1: tokenB},
		{Key: pBC, Token0: tokenB, Token1: tokenC},
		{Key: pCA, Token0: tokenC, Token1: tokenA},
	} {
		if err := store.Upsert(shallow); err != nil {
			t.Fatalf("upsert %s: %v", shallow.Key.Address.Hex(), err)
		}
		// quoteOnChain requires deep state even for a V2 pool.
		if err := store.SetDeep(shallow.Key, pool.DeepState{}); err != nil {
			t.Fatalf("set deep %s: %v", shallow.Key.Address.Hex(), err)
		}
	}

	dirty := []domain.PoolKey{pAB, pBC, pCA}
	onchain := onChainData{
		Input: big.NewInt(1000),
		Path: []poolSpec{
			{Address: pAB.Address, Kind: pAB.Kind, Token0: tokenA, Token1: tokenB},
			{Address: pBC.Address, Kind: pBC.Kind, Token0: tokenB, Token1: tokenC},
			{Address: pCA.Address, Kind: pCA.Kind, Token0: tokenC, Token1: tokenA},
		},
		Legs: []swapLeg{{TokenIn: tokenA}, {TokenIn: tokenB}, {TokenIn: tokenC}},
	}
	return store, dirty, graph.Cycle{Pools: dirty, Start: tokenA, Len: 3}, onchain
}

func TestEvaluateRepeatedlyMeasuresOnlyMeasuredCalls(t *testing.T) {
	store, dirty, cycle, _ := measureFixture(t)

	inner := &fixedQuoter{out: big.NewInt(42)}
	timed := &timedQuoter{inner: inner}
	engine, err := arb.New(stubTable{cycle: cycle}, func(domain.Address) *big.Int {
		return big.NewInt(100)
	}, 1, timed)
	if err != nil {
		t.Fatalf("arb.New: %v", err)
	}

	opportunities, search, err := evaluateRepeatedly(context.Background(), engine, store, dirty, 2, 3, timed)
	if err != nil {
		t.Fatalf("evaluateRepeatedly: %v", err)
	}
	if len(search) != 3 {
		t.Errorf("search samples = %d, want 3", len(search))
	}
	if inner.calls != 15 {
		t.Errorf("quoter calls = %d, want 15 (2 warmup + 3 measured, 3 legs each)", inner.calls)
	}
	if got := len(timed.snapshot()); got != 9 {
		t.Errorf("quote samples = %d, want 9 (3 measured evaluations, 3 legs each)", got)
	}
	if len(opportunities) != 1 {
		t.Fatalf("opportunities = %d, want 1", len(opportunities))
	}
	if got := opportunities[0].Input.String(); got != "100" {
		t.Errorf("kept input = %s, want 100", got)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := evaluateRepeatedly(canceled, engine, store, dirty, 0, 1, timed); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled evaluation error = %v, want context.Canceled", err)
	}
}

// TestMeasureUsesConfigCountsInOrder guards against binding the counts from
// cfg.measurements() in reverse: the sample counts recorded by measure must
// equal the flags, so warmup=2 repeat=7 cannot silently become warmup=7
// repeat=2.
func TestMeasureUsesConfigCountsInOrder(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantWarmup int
		wantRepeat int
	}{
		{
			name:       "repeat seven warmup two",
			args:       []string{"-case", "case.json", "-repeat", "7", "-warmup", "2"},
			wantWarmup: 2,
			wantRepeat: 7,
		},
		{
			name:       "zero warmup",
			args:       []string{"-case", "case.json", "-repeat", "4", "-warmup", "0"},
			wantWarmup: 0,
			wantRepeat: 4,
		},
		{
			name:       "timing off",
			args:       []string{"-case", "case.json", "-repeat", "20", "-warmup", "5", "-timing=false"},
			wantWarmup: 0,
			wantRepeat: 1,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg, err := parseFlags(test.args)
			if err != nil {
				t.Fatalf("parseFlags(%q): %v", test.args, err)
			}

			store, dirty, cycle, onchain := measureFixture(t)
			inner := &fixedQuoter{out: big.NewInt(42)}
			timed := &timedQuoter{inner: inner}
			engine, err := arb.New(stubTable{cycle: cycle}, func(domain.Address) *big.Int {
				return big.NewInt(100)
			}, 1, timed)
			if err != nil {
				t.Fatalf("arb.New: %v", err)
			}

			got, err := measure(context.Background(), cfg, engine, store, dirty, onchain, timed)
			if err != nil {
				t.Fatalf("measure: %v", err)
			}

			legs := len(cycle.Pools)
			if n := len(got.Timing.Search); n != test.wantRepeat {
				t.Errorf("search samples = %d, want %d measured evaluations", n, test.wantRepeat)
			}
			if n := len(got.Timing.EngineQuote); n != test.wantRepeat*legs {
				t.Errorf("engine quote samples = %d, want %d measured legs", n, test.wantRepeat*legs)
			}
			if n := len(got.Timing.Direct); n != test.wantRepeat {
				t.Errorf("direct samples = %d, want %d measured quotes", n, test.wantRepeat)
			}
			if n := len(got.Timing.DirectQuote); n != test.wantRepeat*legs {
				t.Errorf("direct quote samples = %d, want %d measured legs", n, test.wantRepeat*legs)
			}
			if want := (test.wantWarmup + test.wantRepeat) * 2 * legs; inner.calls != want {
				t.Errorf("quoter calls = %d, want %d (warmup+measured, engine+direct)", inner.calls, want)
			}
			if got := got.Direct.Output.String(); got != "42" {
				t.Errorf("direct output = %s, want 42", got)
			}
			if len(got.Opportunities) != 1 {
				t.Errorf("opportunities = %d, want 1", len(got.Opportunities))
			}
		})
	}
}

func TestQuoteRepeatedlyMeasuresOnlyMeasuredCalls(t *testing.T) {
	tokenA, tokenB := testAddress(0x31), testAddress(0x32)
	key := domain.PoolKey{Address: testAddress(0x41), Kind: domain.PoolKindV2}

	store := state.New()
	if err := store.Upsert(pool.State{Key: key, Token0: tokenA, Token1: tokenB, Fee: 500}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := store.SetDeep(key, pool.DeepState{}); err != nil {
		t.Fatalf("set deep: %v", err)
	}

	onchain := onChainData{
		Input: big.NewInt(1000),
		Path:  []poolSpec{{Address: key.Address, Kind: key.Kind, Token0: tokenA, Token1: tokenB}},
		Legs:  []swapLeg{{TokenIn: tokenA}},
	}

	inner := &fixedQuoter{out: big.NewInt(42)}
	timed := &timedQuoter{inner: inner}

	quote, samples, err := quoteRepeatedly(context.Background(), timed, store, onchain, 2, 3)
	if err != nil {
		t.Fatalf("quoteRepeatedly: %v", err)
	}
	if len(samples) != 3 {
		t.Errorf("direct samples = %d, want 3", len(samples))
	}
	if inner.calls != 5 {
		t.Errorf("quoter calls = %d, want 5 (2 warmup + 3 measured)", inner.calls)
	}
	if got := len(timed.snapshot()); got != 3 {
		t.Errorf("quote samples = %d, want 3 measured legs", got)
	}
	if got := quote.Output.String(); got != "42" {
		t.Errorf("direct output = %s, want 42", got)
	}
	if got := quote.Net.String(); got != "-958" {
		t.Errorf("direct net = %s, want -958", got)
	}
	if len(quote.Legs) != 1 {
		t.Errorf("direct legs = %d, want 1", len(quote.Legs))
	}
}
