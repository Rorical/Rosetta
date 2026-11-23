package arb

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Match production snapshot getters: every access returns owned integers/maps.
type copyingQuoteView struct {
	*fakeView
	poolReads, deepReads int
}

func (v *copyingQuoteView) Pool(key domain.PoolKey) (pool.State, bool) {
	v.poolReads++
	p, ok := v.fakeView.Pool(key)
	return p.Copy(), ok
}

func (v *copyingQuoteView) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	v.deepReads++
	d, ok := v.fakeView.Deep(key)
	return d.Copy(), ok
}

func exactBenchmarkFixture(tb testing.TB) (*Engine, *copyingQuoteView, RankedSelection) {
	tb.Helper()
	first := testV3State(1, 1, 2, 3000, 10, 1_000_000_000)
	second := testV3State(2, 2, 1, 3000, 10, 1_000_000_000)
	second.Tick = 1000
	var err error
	second.SqrtPriceX96, err = math.TickToSqrtPriceX96(second.Tick)
	if err != nil {
		tb.Fatal(err)
	}
	deep := emptyDeep()
	deep.WindowMin, deep.WindowMax = -163840, 166390
	for word := int16(-64); word <= 64; word++ {
		deep.Bitmap[word] = new(big.Int)
	}
	view := &copyingQuoteView{fakeView: newFakeView().add(first).add(second).addDeep(first.Key, deep).addDeep(second.Key, deep)}
	table, err := graph.BuildTableFromCycles([]graph.Cycle{{Start: first.Token0, Pools: []domain.PoolKey{first.Key, second.Key}, Len: 2}})
	if err != nil {
		tb.Fatal(err)
	}
	adapter, err := uniswapv3.New(3000, 10)
	if err != nil {
		tb.Fatal(err)
	}
	engine, err := New(table, fixedNotional(1000), 1, adapter)
	if err != nil {
		tb.Fatal(err)
	}
	selection, err := engine.Rank(tb.Context(), view, []domain.PoolKey{first.Key}, rankedOptions(first.Token0, 1_000_000))
	if err != nil || len(selection.candidates) != 1 {
		tb.Fatal("fixture ranking", err)
	}
	view.poolReads, view.deepReads = 0, 0
	return engine, view, selection
}

// Includes real V3 amount sizing and snapshot copies for 129 bitmap words per
// pool. Ranking, tick loading, EVM confirmation and transport are excluded.
func BenchmarkOptimizeWithTickSnapshot(b *testing.B) {
	engine, view, selection := exactBenchmarkFixture(b)
	b.ReportAllocs()
	for b.Loop() {
		result, err := engine.Optimize(b.Context(), view, selection)
		if err != nil || len(result.Opportunities) != 1 {
			b.Fatal(result, err)
		}
	}
}
