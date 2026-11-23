package graph

import (
	"errors"
	"runtime"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func denseFixture() []pool.State {
	var states []pool.State
	id := byte(1)
	for a := byte(1); a <= 6; a++ {
		for b := a + 1; b <= 6; b++ {
			// Reversed token order on some pools exercises both hop directions.
			if (a+b)%3 == 0 {
				states = append(states, testPool(id, b, a))
			} else {
				states = append(states, testPool(id, a, b))
			}
			id++
			if b == a+1 {
				states = append(states, testPool(id, a, b))
				id++
			}
		}
	}
	return states
}

func tableKeys(table *Table) []string {
	keys := make([]string, table.Len())
	for i := range keys {
		keys[i] = cycleKey(table.Cycle(i))
	}
	return keys
}

// The packed build is the plain build packed: same paths in the same order,
// complete or truncated, whatever the number of enumeration workers.
func TestPackedBalancedBuildMatchesThePlainBuild(t *testing.T) {
	states := denseFixture()
	g := mustBuild(t, states...)
	for _, budget := range []int{1_000_000, 300, 60} {
		plain, plainErr := BuildBalancedTableContext(t.Context(), g, 4, budget)
		want, err := PackTable(t.Context(), plain, states)
		if err != nil {
			t.Fatal(err)
		}
		for _, workers := range []int{1, 8} {
			previous := runtime.GOMAXPROCS(workers)
			packed, packedErr := BuildPackedBalancedContext(t.Context(), g, states, 4, budget)
			runtime.GOMAXPROCS(previous)
			if !errors.Is(packedErr, ErrTableFull) && packedErr != nil || errors.Is(packedErr, ErrTableFull) != errors.Is(plainErr, ErrTableFull) {
				t.Fatal("budget", budget, "errors", plainErr, packedErr)
			}
			if !slices.Equal(tableKeys(want), tableKeys(packed)) {
				t.Fatalf("budget %d, %d workers: packed paths differ from the plain build", budget, workers)
			}
			for i := range want.Len() {
				if !slices.Equal(want.PackedLayout().Path(i), packed.PackedLayout().Path(i)) {
					t.Fatalf("budget %d: path %d hops differ", budget, i)
				}
			}
		}
	}
	if full := mustBuildTable(t, g, 4, 1_000_000); full.Len() <= 300 {
		t.Fatal("fixture must exceed the truncated budgets", full.Len())
	}
}

// A packed extension keeps every base index and appends what the plain
// extension appends.
func TestPackedExtensionMatchesThePlainExtension(t *testing.T) {
	states := denseFixture()
	base, err := BuildPackedBalancedContext(t.Context(), mustBuild(t, states...), states, 4, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	added := []pool.State{testPool(90, 7, 1), testPool(91, 7, 2), testPool(92, 3, 7)}
	expanded := append(slices.Clone(states), added...)
	g := mustBuild(t, expanded...)
	var keys []domain.PoolKey
	for _, p := range added {
		keys = append(keys, p.Key)
	}
	plain, err := ExtendTableContext(t.Context(), g, base, keys, 4, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	want, err := PackTable(t.Context(), plain, expanded)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ExtendPackedContext(t.Context(), g, expanded, base, keys, 4, 1_000_000)
	if err != nil {
		t.Fatal(err)
	}
	if got.Len() <= base.Len() || !slices.Equal(tableKeys(want), tableKeys(got)) {
		t.Fatal("packed extension differs from the plain one", got.Len(), base.Len())
	}
	if !slices.Equal(tableKeys(base), tableKeys(got)[:base.Len()]) {
		t.Fatal("extension moved base paths")
	}
	if _, err := ExtendPackedContext(t.Context(), g, expanded, base, keys, 4, base.Len()+1); !errors.Is(err, ErrTableFull) {
		t.Fatal("a budget below the extension must report a full table", err)
	}
}
