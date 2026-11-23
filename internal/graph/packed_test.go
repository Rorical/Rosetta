package graph

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestPackedTablePreservesCyclesPostingsDependenciesAndExtension(t *testing.T) {
	var states []pool.State
	for i, pair := range [][2]byte{{1, 2}, {1, 2}, {2, 3}, {3, 1}, {4, 5}} {
		states = append(states, pool.State{Key: domain.PoolKey{Address: domain.Address{byte(i + 1)}, Kind: domain.PoolKindV2}, Token0: domain.Address{pair[0]}, Token1: domain.Address{pair[1]}})
	}
	g, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildTable(g, 6, 100)
	if err != nil {
		t.Fatal(err)
	}
	packed, err := PackTable(t.Context(), base, states)
	if err != nil {
		t.Fatal(err)
	}
	if packed.cycles != nil || packed.byPool != nil {
		t.Fatal("packed table retains original paths or postings")
	}
	for i := 0; i < base.Len(); i++ {
		if cycleKey(base.Cycle(i)) != cycleKey(packed.Cycle(i)) {
			t.Fatal("path identity changed")
		}
	}
	for _, p := range states {
		for _, k := range []int{0, 1, 100} {
			dirty := []domain.PoolKey{p.Key, p.Key}
			if !slices.Equal(base.Candidates(dirty, k), packed.Candidates(dirty, k)) {
				t.Fatal("postings changed")
			}
		}
		dirty := []domain.PoolKey{p.Key}
		if !slices.Equal(Dependencies(base, dirty), Dependencies(packed, dirty)) {
			t.Fatal("dependency closure changed")
		}
	}
	copy := packed.Cycle(0)
	copy.Pools[0] = domain.PoolKey{}
	if packed.Cycle(0).Pools[0] == (domain.PoolKey{}) {
		t.Fatal("exposed mutable packed storage")
	}
	more := pool.State{Key: domain.PoolKey{Address: domain.Address{9}, Kind: domain.PoolKindV2}, Token0: domain.Address{4}, Token1: domain.Address{1}}
	states = append(states, more)
	g, err = Build(states)
	if err != nil {
		t.Fatal(err)
	}
	extended, err := ExtendTableContext(t.Context(), g, packed, []domain.PoolKey{more.Key}, 6, 100)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < base.Len(); i++ {
		if cycleKey(extended.Cycle(i)) != cycleKey(base.Cycle(i)) {
			t.Fatal("packed extension changed old IDs")
		}
	}
	if packed.Len() != base.Len() {
		t.Fatal("extension mutated base")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := PackTable(ctx, base, states); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
}
