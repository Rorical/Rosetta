package graph

import (
	"math/rand/v2"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestCandidateBufferPreservesDeepAndMergedPostingCaps(t *testing.T) {
	states := []struct{ id, a, b byte }{{1, 1, 2}, {2, 1, 2}, {3, 2, 3}, {4, 3, 1}, {5, 1, 3}}
	var pools []pool.State
	var keys []domain.PoolKey
	for _, row := range states {
		p := testPool(row.id, row.a, row.b)
		pools = append(pools, p)
		keys = append(keys, p.Key)
	}
	g := mustBuild(t, pools...)
	table := mustBuildTable(t, g, 3, 1000)
	deep, err := BuildDeepTable(g, 3, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := NewMergedTable([]CycleSource{deep, table, deep})
	if err != nil {
		t.Fatal(err)
	}
	var buffer CandidateBuffer
	for range 3 {
		for _, source := range []CycleSource{merged, table, deep} {
			for _, dirty := range [][]domain.PoolKey{keys, keys[:1], {keys[2], keys[2]}, {{Kind: domain.PoolKindV4}}, nil} {
				for _, k := range []int{-1, 0, 1, 2, 1000} {
					want := source.Candidates(dirty, k)
					got := buffer.Select(source, dirty, k)
					if !slices.Equal(got, want) || (got == nil) != (want == nil) {
						t.Fatalf("buffer changed source cap/order: got=%v want=%v", got, want)
					}
				}
			}
		}
	}
}

// Spreading the walk over workers returns exactly the serial selection.
func TestSelectWorkersMatchesSelect(t *testing.T) {
	states := denseFixture()
	table, err := BuildPackedBalancedContext(t.Context(), mustBuild(t, states...), states, 4, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	random := rand.New(rand.NewPCG(3, 5))
	var serial, parallel CandidateBuffer
	for trial := range 200 {
		var dirty []domain.PoolKey
		for range 1 + random.IntN(len(states)) {
			dirty = append(dirty, states[random.IntN(len(states))].Key)
		}
		k := []int{1, 7, table.Len()}[trial%3]
		want := slices.Clone(serial.Select(table, dirty, k))
		got := parallel.selectWorkers(table, dirty, k, 1+trial%8, 0)
		if !slices.Equal(want, got) {
			t.Fatalf("trial %d: %d workers selected %d paths, serial %d", trial, 1+trial%8, len(got), len(want))
		}
	}
}
