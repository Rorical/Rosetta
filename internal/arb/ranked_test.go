package arb

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func rankedOptions(token domain.Address, maximum int64) RankedOptions {
	return RankedOptions{
		Bounds: func(asset domain.Address) (InputBounds, bool) {
			return InputBounds{Min: big.NewInt(1), Max: big.NewInt(maximum)}, asset == token
		},
		SwapFee: func(p pool.State, _ domain.Address) (uint32, error) { return p.Fee, nil },
	}
}

func explicitTable(t *testing.T, start domain.Address, sequences ...[]domain.PoolKey) *graph.Table {
	t.Helper()
	cycles := make([]graph.Cycle, len(sequences))
	for i, keys := range sequences {
		cycles[i] = graph.Cycle{Start: start, Pools: keys, Len: len(keys)}
	}
	table, err := graph.BuildTableFromCycles(cycles)
	if err != nil {
		t.Fatal(err)
	}
	return table
}

func TestRankedSearchScoresBeforeK(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	bad := testV2State(0x20, 2, 1, 2_000_000, 500_000)
	winner := testV2State(0x30, 2, 1, 1_000_000, 2_000_000)
	table := explicitTable(t, first.Token0, []domain.PoolKey{first.Key, bad.Key}, []domain.PoolKey{first.Key, winner.Key})
	engine := mustEngine(t, table, 1000, 1, v2CurveQuoter())
	view := newFakeView().add(first).add(bad).add(winner)
	result, err := engine.EvaluateRanked(context.Background(), view, []domain.PoolKey{first.Key}, rankedOptions(first.Token0, 1_000_000))
	if err != nil || result.Considered != 2 || result.Selected != 1 || len(result.Opportunities) != 1 {
		t.Fatalf("ranked result=%+v error=%v", result, err)
	}
	if result.Opportunities[0].Cycle.Pools[1] != winner.Key {
		t.Fatal("enumeration order consumed K before profit ranking")
	}
}

func TestRankedMultiHopFindsProfitWhenFixedNotionalLoses(t *testing.T) {
	states := []pool.State{
		testV2State(0x10, 1, 2, 1000, 2000),
		testV2State(0x20, 2, 3, 1000, 2000),
		testV2State(0x30, 3, 1, 1000, 2000),
	}
	keys := []domain.PoolKey{states[0].Key, states[1].Key, states[2].Key}
	table := explicitTable(t, states[0].Token0, keys)
	quoter := v2CurveQuoter()
	engine := mustEngine(t, table, 5000, 1, quoter)
	view := newFakeView().add(states[0]).add(states[1]).add(states[2])
	fixed, err := engine.Evaluate(context.Background(), view, keys[:1])
	if err != nil || len(fixed) != 1 || fixed[0].Net.Sign() >= 0 {
		t.Fatal("fixture's fixed input must lose")
	}
	quoter.calls = nil
	result, err := engine.EvaluateRanked(context.Background(), view, keys[:1], rankedOptions(states[0].Token0, 5000))
	if err != nil || len(result.Opportunities) != 1 {
		t.Fatalf("optimized result=%+v error=%v", result, err)
	}
	if len(quoter.calls) > (36+64+8)*len(keys) {
		t.Fatalf("unbounded sizing work: %d leg quotes", len(quoter.calls))
	}
	best := new(big.Int)
	for input := int64(1); input <= 5000; input++ {
		amount := big.NewInt(input)
		for _, p := range states {
			amount, err = math.QuoteV2ExactIn(p.Reserve0, p.Reserve1, p.Fee, amount)
			if err != nil {
				t.Fatal(err)
			}
		}
		net := new(big.Int).Sub(amount, big.NewInt(input))
		if net.Cmp(best) > 0 {
			best.Set(net)
		}
	}
	// Integer rounding can create tiny local plateaus; compare profit to an
	// independent exhaustive search, rather than asserting the same input.
	if delta := new(big.Int).Sub(best, result.Opportunities[0].Net); delta.Sign() < 0 || delta.Cmp(big.NewInt(2)) > 0 {
		t.Fatalf("optimized net=%s exhaustive=%s", result.Opportunities[0].Net, best)
	}
}

func TestRankedSearchRotatesToFundingAssetAndSeparatesDenominations(t *testing.T) {
	first := testV2State(0x10, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	table := explicitTable(t, first.Token0, []domain.PoolKey{first.Key, second.Key})
	engine := mustEngine(t, table, 1000, 1, v2CurveQuoter())
	view := newFakeView().add(first).add(second)
	options := rankedOptions(first.Token1, 100_000)
	result, err := engine.EvaluateRanked(context.Background(), view, []domain.PoolKey{first.Key}, options)
	if err != nil || len(result.Opportunities) != 1 || result.Opportunities[0].Cycle.Start != first.Token1 || result.Opportunities[0].Cycle.Pools[0] != second.Key {
		t.Fatalf("funding asset rotation failed: %+v %v", result, err)
	}
	options.Bounds = func(asset domain.Address) (InputBounds, bool) {
		cap := int64(100)
		if asset == first.Token1 {
			cap = 100_000
		}
		return InputBounds{Min: big.NewInt(1), Max: big.NewInt(cap)}, true
	}
	result, err = engine.EvaluateRanked(context.Background(), view, []domain.PoolKey{first.Key}, options)
	if err != nil || len(result.Opportunities) != 2 || result.Selected != 2 {
		t.Fatalf("K should apply per asset: %+v %v", result, err)
	}
	if result.Opportunities[0].Cycle.Start != first.Token0 || result.Opportunities[0].Net.Cmp(result.Opportunities[1].Net) >= 0 {
		t.Fatal("different profit denominations were compared by raw amount")
	}
}

func TestRankedSearchReportsMissingAndOutOfWindowTicks(t *testing.T) {
	first := testV3State(0x10, 1, 2, 3000, 10, 1_000_000)
	second := testV2State(0x20, 2, 1, 1_000_000, 2_000_000)
	table := explicitTable(t, first.Token0, []domain.PoolKey{first.Key, second.Key})
	quoter := &fakeQuoter{quote: func(pool.State, pool.DeepState, domain.Address, *big.Int) (*big.Int, error) {
		return nil, math.ErrOutOfWindow
	}}
	engine := mustEngine(t, table, 1000, 1, quoter)
	view := newFakeView().add(first).add(second)
	for _, want := range []error{ErrMissingDeep, math.ErrOutOfWindow} {
		result, err := engine.EvaluateRanked(context.Background(), view, []domain.PoolKey{first.Key}, rankedOptions(first.Token0, 10000))
		if err != nil || len(result.Opportunities) != 0 || len(result.Issues) != 1 || !errors.Is(result.Issues[0].Err, want) {
			t.Fatalf("missing dependency lost: result=%+v error=%v want=%v", result, err, want)
		}
		view.addDeep(first.Key, emptyDeep())
	}
}
