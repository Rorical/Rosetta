package arb

import (
	"reflect"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestRankSelectionSurvivesWorkspaceReuseAndFundingChanges(t *testing.T) {
	first := testV2State(1, 1, 2, 1_000_000, 2_000_000)
	second := testV2State(2, 2, 1, 1_000_000, 2_000_000)
	engine := mustEngine(t, mustTable(t, 2, first, second), 1000, 1, identityQuoter())
	view := newFakeView().add(first).add(second)
	dirty := []domain.PoolKey{first.Key}
	opts := rankedOptions(first.Token0, 100000)
	selection, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for range 30 {
				rotated, err := engine.Rank(t.Context(), view, dirty, rankedOptions(first.Token1, 100))
				if err != nil || len(rotated.candidates) != 1 || rotated.candidates[0].cycle.Pools[0] != second.Key {
					t.Error("funded path changed during reuse", err)
					return
				}
				rotated.candidates[0].bounds.Max.SetInt64(2)
				rotated.candidates[0].cycle.Pools[0] = domain.PoolKey{}
			}
		})
	}
	workers.Wait()
	again, err := engine.Rank(t.Context(), view, dirty, opts)
	if err != nil || !reflect.DeepEqual(selection, again) {
		t.Fatal("previous selection aliases pooled paths or bounds", err)
	}
}
