package metricomm

import (
	"context"
	"errors"
	poolmath "github.com/Rorical/Rosetta/internal/math"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

var (
	testToken0 = domain.Address{0xa0}
	testToken1 = domain.Address{0xa1}
)

// fixturePoolState builds the shared pool state the adapter reads from
// recorded storage and a quote.
func fixturePoolState(t *testing.T, im fixtureImmutables, storage fixtureStorage, bid, ask *big.Int) pool.State {
	t.Helper()
	words := []domain.Hash{mustHash(t, storage.Slot0), mustHash(t, storage.Slot1)}
	for _, bin := range storage.Bins {
		words = append(words, mustHash(t, bin.Word))
	}
	return pool.State{
		Key:    domain.PoolKey{Address: domain.Address{0x01}, Kind: domain.PoolKindMetricOmm},
		Token0: testToken0,
		Token1: testToken1,
		Metric: &pool.MetricState{
			Token0Scale: mustBig(t, im.Token0ScaleMultiplier),
			Token1Scale: mustBig(t, im.Token1ScaleMultiplier),
			LowestBin:   im.LowestBin,
			HighestBin:  im.HighestBin,
			Words:       words,
			BidX64:      bid,
			AskX64:      ask,
		},
	}
}

func noLimit(zeroForOne bool) *big.Int {
	if zeroForOne {
		return new(big.Int)
	}
	return noBuyPriceLimit
}

// TestAdapterMatchesSwapAndChain quotes every unlimited exact-input case of
// the live fixtures. The adapter must return exactly what Swap computes with
// the adapter's own reference, reject what the pool fills only in part, and
// equal the chain's output, although the chain swapped on another reference
// inside the same bid and ask.
func TestAdapterMatchesSwapAndChain(t *testing.T) {
	adapter := New()
	ctx := context.Background()
	for _, f := range loadFixtures[liveFixture](t, isLive) {
		t.Run(f.Pool, func(t *testing.T) {
			var quoted, partial int
			for _, c := range f.Cases {
				outcome := c.fixtureOutcome
				if outcome.Outcome != "ok" && c.Stubbed != nil {
					outcome = *c.Stubbed
				}
				amount, limit := mustBig(t, c.AmountSpecified), mustBig(t, c.PriceLimitX64)
				if outcome.Outcome != "ok" || amount.Sign() <= 0 || limit.Cmp(noLimit(c.ZeroForOne)) != 0 {
					continue
				}
				bid, ask := mustBig(t, c.BidX64), mustBig(t, c.AskX64)
				state := fixturePoolState(t, f.Immutables, f.Storage, bid, ask)
				tokenIn, chainIn, chainOut := testToken1, mustBig(t, outcome.Amount1Delta), mustBig(t, outcome.Amount0Delta)
				if c.ZeroForOne {
					tokenIn, chainIn, chainOut = testToken0, chainOut, chainIn
				}
				chainOut.Neg(chainOut)

				got, err := adapter.QuoteExactIn(ctx, state, pool.DeepState{}, tokenIn, amount)
				if chainIn.Cmp(amount) != 0 {
					// The search can size a route beyond the pool's inventory; treating
					// that as a dependency needing a refresh repeats it every probe of
					// every block. An input the pool cannot fill is a sizing bound like
					// a V3 pool's.
					if !errors.Is(err, poolmath.ErrCapacity) {
						t.Fatalf("partial fill is not a capacity bound: %v", err)
					}
					if !errors.Is(err, ErrPartialFill) {
						t.Errorf("%s: chain took %s of %s, adapter returned (%v, %v)", c.Name, chainIn, amount, got, err)
					}
					partial++
					continue
				}
				if err != nil {
					t.Errorf("%s: %v", c.Name, err)
					continue
				}

				decoded := decodeFixtureState(t, f.Immutables, f.Storage, nil)
				reference := new(big.Int).Sqrt(new(big.Int).Mul(bid, ask))
				amount0, amount1, _, err := Swap(decoded, c.ZeroForOne, amount, limit, bid, ask, reference)
				if err != nil {
					t.Fatalf("%s: %v", c.Name, err)
				}
				want := amount0
				if c.ZeroForOne {
					want = amount1
				}
				if want.Neg(want).Cmp(got) != 0 {
					t.Errorf("%s: adapter %s, Swap %s", c.Name, got, want)
				}

				if got.Cmp(chainOut) != 0 {
					t.Errorf("%s: adapter %s, chain %s on its own reference", c.Name, got, chainOut)
				}
				quoted++
			}
			t.Logf("%d quotes equal Swap and the chain's output, %d partial fills rejected", quoted, partial)
			if quoted < 50 || partial == 0 {
				t.Fatalf("fixture exercised %d quotes and %d partial fills", quoted, partial)
			}
		})
	}
}

func TestAdapterRejectsStatesItCannotQuote(t *testing.T) {
	live := loadFixtures[liveFixture](t, isLive)[0]
	c := live.Cases[0]
	bid, ask := mustBig(t, c.BidX64), mustBig(t, c.AskX64)
	amount := big.NewInt(1_000_000_000_000_000)
	base := func() pool.State { return fixturePoolState(t, live.Immutables, live.Storage, bid, ask) }
	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	tests := []struct {
		name    string
		ctx     context.Context
		mutate  func(*pool.State)
		tokenIn domain.Address
		amount  *big.Int
		want    error
	}{
		{"canceled context", canceled, func(*pool.State) {}, testToken0, amount, context.Canceled},
		{"another pool kind", nil, func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 }, testToken0, amount, pool.ErrInvalidState},
		{"token outside the pool", nil, func(*pool.State) {}, domain.Address{0xff}, amount, pool.ErrInvalidState},
		{"nil amount", nil, func(*pool.State) {}, testToken0, nil, ErrInvalidInput},
		{"zero amount", nil, func(*pool.State) {}, testToken0, new(big.Int), ErrInvalidInput},
		{"negative amount", nil, func(*pool.State) {}, testToken0, big.NewInt(-5), ErrInvalidInput},
		{"no metric state", nil, func(s *pool.State) { s.Metric = nil }, testToken0, amount, pool.ErrInvalidState},
		{"stale words", nil, func(s *pool.State) { s.Metric.Stale = true }, testToken0, amount, ErrStaleState},
		{"no bid", nil, func(s *pool.State) { s.Metric.BidX64 = nil }, testToken0, amount, ErrNoQuote},
		{"no ask", nil, func(s *pool.State) { s.Metric.AskX64 = nil }, testToken1, amount, ErrNoQuote},
		{"paused pool", nil, func(s *pool.State) { s.Metric.Words[0][31] = 1 }, testToken0, amount, ErrPoolPaused},
		{"missing bin word", nil, func(s *pool.State) { s.Metric.Words = s.Metric.Words[1:] }, testToken0, amount, ErrInvalidState},
		{"no scale multiplier", nil, func(s *pool.State) { s.Metric.Token1Scale = nil }, testToken0, amount, pool.ErrInvalidState},
		{"bid above ask", nil, func(s *pool.State) { s.Metric.BidX64, s.Metric.AskX64 = ask, bid }, testToken0, amount, ErrBidGreaterThanAsk},
		{"more than the pool can fill", nil, func(*pool.State) {}, testToken0, new(big.Int).Lsh(big.NewInt(1), 100), ErrPartialFill},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := base()
			test.mutate(&state)
			ctx := test.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			got, err := New().QuoteExactIn(ctx, state, pool.DeepState{}, test.tokenIn, test.amount)
			if !errors.Is(err, test.want) || got != nil {
				t.Fatalf("got (%v, %v), want %v", got, err, test.want)
			}
		})
	}

	state := base()
	before := state.Copy()
	if _, err := New().QuoteExactIn(context.Background(), state, pool.DeepState{}, testToken0, amount); err != nil {
		t.Fatal(err)
	}
	for i := range before.Metric.Words {
		if state.Metric.Words[i] != before.Metric.Words[i] {
			t.Fatalf("quote changed storage word %d", i)
		}
	}
	if state.Metric.BidX64.Cmp(bid) != 0 || state.Metric.AskX64.Cmp(ask) != 0 {
		t.Fatal("quote changed the bid or ask")
	}
}
