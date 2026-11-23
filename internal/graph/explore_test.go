package graph

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestExplorerResumesEveryEdgeAndSkipsIndexedCycles(t *testing.T) {
	var states []pool.State
	for i, pair := range [][2]byte{{1, 2}, {1, 2}, {2, 3}, {3, 1}, {2, 4}, {4, 1}} {
		states = append(states, pool.State{Key: domain.PoolKey{Address: domain.Address{byte(i + 1)}, Kind: domain.PoolKindV2}, Token0: domain.Address{pair[0]}, Token1: domain.Address{pair[1]}})
	}
	g, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	full, err := BuildTable(g, 4, 1000)
	if err != nil {
		t.Fatal(err)
	}
	indexed, err := BuildTableFromCycles([]Cycle{full.Cycle(0)})
	if err != nil {
		t.Fatal(err)
	}
	indexed, err = PackTable(t.Context(), indexed, states)
	if err != nil {
		t.Fatal(err)
	}
	explorer, err := NewExplorer(g, indexed, ExploreLimits{MaxDepth: 4, Steps: 1, NewPaths: 1, CachePaths: 1000})
	if err != nil {
		t.Fatal(err)
	}
	var dirty []domain.PoolKey
	for _, p := range states {
		dirty = append(dirty, p.Key)
	}
	var result Exploration
	for turn := 0; turn < 10000; turn++ {
		result, err = explorer.Explore(t.Context(), dirty)
		if err != nil {
			t.Fatal(err)
		}
		if result.Steps > 1 || result.Added > 1 || len(result.Cycles) > 1000 {
			t.Fatal("exceeded work/cache limit")
		}
		if !result.BudgetExhausted {
			break
		}
	}
	if result.BudgetExhausted {
		t.Fatal("resumable exploration failed to finish")
	}
	want := map[string]bool{}
	for i := 0; i < full.Len(); i++ {
		cycle := full.Cycle(i)
		if !indexed.ContainsCycle(cycle) {
			want[cycleKey(cycle)] = true
		}
	}
	if len(want) != len(result.Cycles) {
		t.Fatalf("found %d of %d unindexed paths", len(result.Cycles), len(want))
	}
	for _, cycle := range result.Cycles {
		if !want[cycleKey(cycle)] {
			t.Fatal("wrong identity or indexed duplicate")
		}
	}
	bounded, err := NewExplorer(g, indexed, ExploreLimits{MaxDepth: 4, Steps: 64, NewPaths: 2, CachePaths: 2})
	if err != nil {
		t.Fatal(err)
	}
	seen := make(map[string]bool)
	evicted := false
	for turn := 0; turn < 500; turn++ {
		result, err := bounded.Explore(t.Context(), dirty)
		if err != nil {
			t.Fatal(err)
		}
		if result.Steps > 64 || result.Added > 2 || len(bounded.cache) > 2 {
			t.Fatal("eviction bypassed limits")
		}
		evicted = evicted || result.CacheTruncated
		for _, cycle := range result.Cycles {
			seen[cycleKey(cycle)] = true
		}
	}
	if !evicted || len(seen) != len(want) {
		t.Fatal("evicted paths never returned during continued exploration", len(seen), len(want))
	}
	before := explorer.next
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := explorer.Explore(ctx, dirty); !errors.Is(err, context.Canceled) || explorer.next != before {
		t.Fatal("canceled traversal mutated cursor", err)
	}
}

func TestExplorerFindsEightHopMarketOutsideFullSixHopTable(t *testing.T) {
	var states []pool.State
	for i := byte(1); i <= 4; i++ {
		states = append(states, pool.State{Key: domain.PoolKey{Address: domain.Address{i}, Kind: domain.PoolKindV2}, Token0: domain.Address{1}, Token1: domain.Address{2}})
	}
	var ring []domain.PoolKey
	for i := byte(0); i < 8; i++ {
		from, to := domain.Address{10 + i}, domain.Address{11 + i}
		if i == 0 {
			from = domain.Address{1}
		}
		if i == 7 {
			to = domain.Address{1}
		}
		p := pool.State{Key: domain.PoolKey{Address: domain.Address{20 + i}, Kind: domain.PoolKindV2}, Token0: from, Token1: to}
		states = append(states, p)
		ring = append(ring, p.Key)
	}
	g, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	indexed, err := BuildBalancedTableContext(t.Context(), g, 6, 4)
	if !errors.Is(err, ErrTableFull) {
		t.Fatal("main table should be full", err)
	}
	indexed, err = PackTable(t.Context(), indexed, states)
	if err != nil {
		t.Fatal(err)
	}
	explorer, err := NewExplorer(g, indexed, ExploreLimits{MaxDepth: 8, Steps: 8, NewPaths: 1, CachePaths: 2})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for turn := 0; turn < 2000; turn++ {
		result, err := explorer.Explore(t.Context(), []domain.PoolKey{ring[0]})
		if err != nil {
			t.Fatal(err)
		}
		if result.Steps > 8 || result.Added > 1 || len(explorer.cache) > 2 {
			t.Fatal("unbounded exploration")
		}
		for _, cycle := range result.Cycles {
			if slices.Equal(cycle.Pools, ring) {
				found = true
			}
		}
		if found {
			break
		}
	}
	if !found {
		t.Fatal("full indexed table hid unindexed eight-hop market")
	}
	if indexed.Len() != 4 {
		t.Fatal("explorer mutated main path budget")
	}
}
