package arb

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestPackedIncrementalInputsMatchFreshRankingAcrossMissingPoolsFeesAndFunding(t *testing.T) {
	states := []pool.State{testV2State(1, 1, 2, 1_000_000, 1_000_000), testV2State(2, 2, 1, 1_000_000, 1_000_000), testV2State(3, 2, 1, 1_000_000, 2_000_000), testV2State(4, 3, 4, 1_000_000, 1_000_000)}
	table, err := graph.PackTable(t.Context(), mustTable(t, 2, states...), states)
	if err != nil {
		t.Fatal(err)
	}
	engine := mustEngine(t, table, 1000, 2, identityQuoter())
	if err := engine.PreparePaths(t.Context(), states); err != nil {
		t.Fatal(err)
	}
	if &engine.compiled.hops[0] != &table.PackedLayout().Hops[0] || &engine.compiled.paths[0] != &table.PackedLayout().Paths[0] {
		t.Fatal("engine copied packed path arrays")
	}
	view := newFakeView()
	for _, p := range states {
		view.add(p)
	}
	dirty := []domain.PoolKey{states[0].Key}
	var feeError error
	bounds := InputBounds{Min: big.NewInt(1), Max: big.NewInt(100000)}
	enabled := true
	opts := RankedOptions{Bounds: func(token domain.Address) (InputBounds, bool) { return bounds, enabled && token == states[0].Token0 }, SwapFee: func(p pool.State, token domain.Address) (uint32, error) {
		if p.Key == states[1].Key && token == states[1].Token0 {
			return p.Fee, feeError
		}
		return p.Fee, nil
	}}
	var cache rankCache
	cache.bind(engine.compiled, 1)
	check := func() {
		t.Helper()
		want, err := engine.Rank(t.Context(), view, dirty, opts)
		if err != nil {
			t.Fatal(err)
		}
		got, err := engine.rank(t.Context(), view, dirty, opts, &cache)
		if err != nil {
			t.Fatal(err)
		}
		assertSameRanking(t, want, got)
	}
	check()
	check()
	// An unrelated dirty set must not assume every pool was initialized already.
	dirty = []domain.PoolKey{states[3].Key}
	check()
	dirty = []domain.PoolKey{states[0].Key}
	delete(view.states, states[1].Key)
	check()
	view.add(states[1])
	check()
	feeError = errors.New("directional fee unavailable")
	check()
	cache.bind(engine.compiled, 2)
	check()
	feeError = nil
	check()
	enabled = false
	check()
	enabled = true
	check()
	bounds.Max = big.NewInt(10)
	check()
	// Rejection caches must not hide invalid changed funding bounds.
	for _, p := range states[:3] {
		p.Reserve1.Set(p.Reserve0)
	}
	check()
	bounds.Min = big.NewInt(0)
	if _, err := engine.rank(t.Context(), view, dirty, opts, &cache); !errors.Is(err, ErrInvalidConfig) {
		t.Fatal("cached negative hid invalid funding", err)
	}
	bounds.Min = big.NewInt(1)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := engine.rank(ctx, view, dirty, opts, &cache); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	check()
}
