package graph

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestBalancedTableKeepsCompleteSetAndDeterministicOrder(t *testing.T) {
	var states []pool.State
	for i, pair := range [][2]byte{{1, 2}, {1, 2}, {2, 3}, {3, 1}, {3, 4}, {4, 1}} {
		states = append(states, pool.State{Key: domain.PoolKey{Address: domain.Address{byte(i + 1)}, Kind: domain.PoolKindV2}, Token0: domain.Address{pair[0]}, Token1: domain.Address{pair[1]}})
	}
	g, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	full, err := BuildTableContext(t.Context(), g, 6, 1000)
	if err != nil {
		t.Fatal(err)
	}
	balanced, err := BuildBalancedTableContext(t.Context(), g, 6, 1000)
	if err != nil {
		t.Fatal(err)
	}
	keys := func(table *Table) []string {
		out := []string{}
		for i := 0; i < table.Len(); i++ {
			out = append(out, cycleKey(table.Cycle(i)))
		}
		return out
	}
	expected, actual := keys(full), keys(balanced)
	slices.Sort(expected)
	slices.Sort(actual)
	if !slices.Equal(expected, actual) {
		t.Fatal("seed quotas dropped or duplicated cycles")
	}
	slices.Reverse(states)
	reversed, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	same, err := BuildBalancedTableContext(t.Context(), reversed, 6, 1000)
	if err != nil || !slices.Equal(keys(balanced), keys(same)) {
		t.Fatal("input order changed balanced identities", err)
	}
	bounded, err := BuildBalancedTableContext(t.Context(), g, 6, 5)
	if !errors.Is(err, ErrTableFull) || bounded.Len() != 5 {
		t.Fatal("budget not preserved", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err = BuildBalancedTableContext(ctx, g, 6, 1000); !errors.Is(err, context.Canceled) {
		t.Fatal("ignored cancellation", err)
	}
}

// A budget that holds every shorter route, plus a seed per pool and direction
// of each longer length, keeps all of them: longer routes take only what is
// left. A global DFS fill with a 500,000 budget can keep about 40,000 of
// 195,000 three-hop routes while four-hop routes fill the rest.
func TestBalancedTableCompletesShorterLengthsFirst(t *testing.T) {
	var states []pool.State
	id := byte(1)
	for a := byte(1); a <= 6; a++ {
		for b := a + 1; b <= 6; b++ {
			copies := 1
			if b == a+1 {
				copies = 2 // parallel pools give two-hop routes
			}
			for range copies {
				states = append(states, testPool(id, a, b))
				id++
			}
		}
	}
	g := mustBuild(t, states...)
	full := mustBuildTable(t, g, 4, 1_000_000)
	short := map[string]bool{}
	for i := range full.Len() {
		if cycle := full.Cycle(i); cycle.Len <= 3 {
			short[cycleKey(cycle)] = true
		}
	}
	if len(short) == 0 || len(short) >= full.Len() {
		t.Fatal("fixture needs routes of every length", len(short), full.Len())
	}
	seeds := 2 * len(states) // one length, four hops, does not fit
	if len(short)+seeds >= full.Len() {
		t.Fatal("fixture must overflow the budget", full.Len())
	}
	table, err := BuildBalancedTableContext(t.Context(), g, 4, len(short)+seeds)
	if !errors.Is(err, ErrTableFull) {
		t.Fatal("a budget below the route count must report a full table", err)
	}
	for i := range table.Len() {
		delete(short, cycleKey(table.Cycle(i)))
	}
	if len(short) != 0 {
		t.Fatalf("%d short routes lost to longer ones", len(short))
	}
}
