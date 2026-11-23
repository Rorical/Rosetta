package arb

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

type certifiedTestQuoter struct{ *fakeQuoter }

func (q certifiedTestQuoter) ExactInputMonotone(pool.State) bool { return true }

type hiddenMonotonicity struct{ Quoter }

func TestSizingDeduplicatesAmountsAndPrunesProvenLosses(t *testing.T) {
	first := testV2State(1, 1, 2, 1000000, 1000000)
	second := testV2State(2, 2, 1, 1000000, 1000000)
	table := explicitTable(t, first.Token0, []domain.PoolKey{first.Key, second.Key})
	view := newFakeView().add(first).add(second)
	candidate := rankedCycle{cycle: table.Cycle(0), bounds: InputBounds{Min: big.NewInt(10000), Max: big.NewInt(100000)}}
	quote := func(p pool.State, _ pool.DeepState, _ domain.Address, input *big.Int) (*big.Int, error) {
		if p.Key == first.Key {
			return new(big.Int).Set(input), nil
		}
		return new(big.Int).Quo(input, big.NewInt(2)), nil
	}
	slow := &fakeQuoter{quote: quote}
	fast := &fakeQuoter{quote: quote}
	baseline := mustEngine(t, table, 10000, 1, slow)
	optimized := mustEngine(t, table, 10000, 1, certifiedTestQuoter{fast})
	a, _, aOK, err := baseline.optimizeCycle(t.Context(), view, candidate)
	if err != nil || !aOK {
		t.Fatal(err)
	}
	b, _, bOK, err := optimized.optimizeCycle(t.Context(), view, candidate)
	if err != nil || !bOK || b.Net.Cmp(a.Net) != 0 || b.Input.Cmp(a.Input) != 0 {
		t.Fatal("loss pruning changed best observed quote", a, b, err)
	}
	t.Logf("losing interval leg quotes: %d -> %d", len(slow.calls), len(fast.calls))
	if len(fast.calls) >= len(slow.calls)/2 {
		t.Fatalf("failed to cut useless sizing: fast=%d baseline=%d", len(fast.calls), len(slow.calls))
	}
	for _, calls := range [][]quoteCall{fast.calls, slow.calls} {
		seen := map[string]bool{}
		for _, call := range calls {
			if call.pool != first.Key.Address {
				continue
			}
			key := call.amount.String()
			if seen[key] {
				t.Fatal("duplicate full route quote", key)
			}
			seen[key] = true
		}
	}
}

func TestNegativeGridStillRefinesHiddenProfitableAmount(t *testing.T) {
	first := testV2State(1, 1, 2, 1000000, 1000000)
	second := testV2State(2, 2, 1, 1000000, 1000000)
	table := explicitTable(t, first.Token0, []domain.PoolKey{first.Key, second.Key})
	quoter := certifiedTestQuoter{&fakeQuoter{quote: func(p pool.State, _ pool.DeepState, _ domain.Address, input *big.Int) (*big.Int, error) {
		if p.Key == first.Key {
			return new(big.Int).Set(input), nil
		}
		switch x := input.Int64(); {
		case x < 64:
			return new(big.Int), nil
		case x < 80:
			return big.NewInt(60), nil
		case x < 128:
			return big.NewInt(110), nil
		default:
			return big.NewInt(120), nil
		}
	}}}
	engine := mustEngine(t, table, 64, 1, quoter)
	candidate := rankedCycle{cycle: table.Cycle(0), bounds: InputBounds{Min: big.NewInt(16), Max: big.NewInt(256)}}
	for _, point := range sizingPoints(candidate.bounds, big.NewInt(64)) {
		quote, err := engine.QuoteCycle(t.Context(), newFakeView().add(first).add(second), candidate.cycle, point)
		if err != nil || quote.Net.Sign() > 0 {
			t.Fatal("grid must contain no profitable sample", point, err)
		}
	}
	best, _, ok, err := engine.optimizeCycle(t.Context(), newFakeView().add(first).add(second), candidate)
	if err != nil || !ok || best.Net.Sign() <= 0 {
		t.Fatal("negative samples pruned a profitable interval", best, err)
	}
}

func TestSizingWindowCeilingPreservesSmallOpportunity(t *testing.T) {
	engine, view, selection := exactBenchmarkFixture(t)
	first := selection.candidates[0].cycle.Pools[0]
	deep := view.deep[first]
	deep.WindowMin = -2
	view.deep[first] = deep
	original := engine.quoter
	run := func(quoter Quoter) (Opportunity, []QuoteIssue, int) {
		calls := 0
		counted := &countedSizingQuoter{Quoter: quoter, calls: &calls}
		// The counted wrapper delegates certification only when the source has it.
		engine.quoter = counted
		best, issues, ok, err := engine.optimizeCycle(t.Context(), view, selection.candidates[0])
		if err != nil || !ok {
			t.Fatal(err)
		}
		return best, issues, calls
	}
	fast, issues, fastCalls := run(original)
	baseline, _, slowCalls := run(hiddenMonotonicity{original})
	if fast.Net.Sign() <= 0 || fast.Net.Cmp(baseline.Net) != 0 || fast.Input.Cmp(baseline.Input) != 0 || fastCalls >= slowCalls {
		t.Fatalf("window pruning: fast=%v baseline=%v calls=%d/%d", fast, baseline, fastCalls, slowCalls)
	}
	t.Logf("bounded window leg quotes: %d -> %d", slowCalls, fastCalls)
	if len(issues) == 0 || !errors.Is(issues[0].Err, math.ErrOutOfWindow) {
		t.Fatal("lost missing window diagnostic", issues)
	}
	// A fresh call must not reuse the failed amount after loading more ticks.
	engine.quoter = original
	deep.WindowMin = -163840
	view.deep[first] = deep
	recovered, _, ok, err := engine.optimizeCycle(t.Context(), view, selection.candidates[0])
	if err != nil || !ok || recovered.Net.Cmp(fast.Net) < 0 {
		t.Fatal("window ceiling escaped invocation", err)
	}
}

// A pool that cannot fill the larger samples bounds the search below them. The
// capacity limit is counted but is not a dependency issue, so the best input
// under it is still found and the path stays cacheable.
func TestSizingCapacityShrinksInputWithoutIssue(t *testing.T) {
	engine, view, selection := exactBenchmarkFixture(t)
	candidate := selection.candidates[0]
	unbounded, _, ok, err := engine.optimizeCycle(t.Context(), view, candidate)
	if err != nil || !ok || unbounded.Net.Sign() <= 0 {
		t.Fatal("fixture has no opportunity", err)
	}
	limit := new(big.Int).Quo(unbounded.Input, big.NewInt(2))
	engine.quoter = capacityQuoter{Quoter: engine.quoter, pool: candidate.cycle.Pools[0], limit: limit}
	var limits QuoteLimits
	best, issues, ok, err := engine.optimizeCycleObserved(t.Context(), view, candidate, nil, &limits)
	if err != nil || !ok || best.Net.Sign() <= 0 || best.Input.Cmp(limit) > 0 {
		t.Fatalf("capacity bound: best=%v input=%v limit=%v err=%v", best.Net, best.Input, limit, err)
	}
	if len(issues) != 0 || limits.Capacity == 0 || limits.OutOfWindow != 0 {
		t.Fatalf("capacity reported as dependency: issues=%v limits=%+v", issues, limits)
	}
}

type capacityQuoter struct {
	Quoter
	pool  domain.PoolKey
	limit *big.Int
}

func (q capacityQuoter) QuoteExactIn(ctx context.Context, s pool.State, d pool.DeepState, token domain.Address, input *big.Int) (*big.Int, error) {
	if s.Key == q.pool && input.Cmp(q.limit) > 0 {
		return nil, fmt.Errorf("fixture: %w", math.ErrCapacity)
	}
	return q.Quoter.QuoteExactIn(ctx, s, d, token, input)
}

func (q capacityQuoter) ExactInputMonotone(s pool.State) bool {
	proof, ok := q.Quoter.(pool.MonotoneQuoter)
	return ok && proof.ExactInputMonotone(s)
}

type countedSizingQuoter struct {
	Quoter
	calls *int
}

func (q *countedSizingQuoter) QuoteExactIn(ctx context.Context, s pool.State, d pool.DeepState, token domain.Address, input *big.Int) (*big.Int, error) {
	(*q.calls)++
	return q.Quoter.QuoteExactIn(ctx, s, d, token, input)
}
func (q *countedSizingQuoter) ExactInputMonotone(s pool.State) bool {
	proof, ok := q.Quoter.(pool.MonotoneQuoter)
	return ok && proof.ExactInputMonotone(s)
}

func TestMonotoneSizingMatchesFullSearchAcrossV3States(t *testing.T) {
	engine, view, selection := exactBenchmarkFixture(t)
	source := engine.quoter
	candidate := selection.candidates[0]
	for sample := range 80 {
		for i, key := range candidate.cycle.Pools {
			p := view.states[key]
			p.Tick = int32((sample*37+i*149)%600 - 300)
			var err error
			p.SqrtPriceX96, err = math.TickToSqrtPriceX96(p.Tick)
			if err != nil {
				t.Fatal(err)
			}
			p.Liquidity = big.NewInt(int64(100_000 + sample*107_351 + i*47_371))
			view.states[key] = p
		}
		candidate.bounds = InputBounds{Min: big.NewInt(int64(1 + sample%7)), Max: new(big.Int).Lsh(big.NewInt(1000), uint(sample%16))}
		current := RankedSelection{candidates: []rankedCycle{candidate}}
		engine.quoter = source
		fast, err := engine.Optimize(t.Context(), view, current)
		if err != nil {
			t.Fatal(err)
		}
		engine.quoter = hiddenMonotonicity{source}
		full, err := engine.Optimize(t.Context(), view, current)
		if err != nil {
			t.Fatal(err)
		}
		if len(fast.Opportunities) != len(full.Opportunities) {
			t.Fatalf("sample %d changed positive recall", sample)
		}
		for i, got := range fast.Opportunities {
			want := full.Opportunities[i]
			if got.Input.Cmp(want.Input) != 0 || got.Output.Cmp(want.Output) != 0 || got.Net.Cmp(want.Net) != 0 {
				t.Fatalf("sample %d changed positive quote: fast=%+v full=%+v", sample, got, want)
			}
		}
	}
}
