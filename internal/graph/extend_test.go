package graph

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestExtendTableFindsNewLongCyclesAndPreservesOldIndex(t *testing.T) {
	var states []pool.State
	for i := byte(1); i < 8; i++ {
		states = append(states, pool.State{Key: domain.PoolKey{Address: domain.Address{i}, Kind: domain.PoolKindV2}, Token0: domain.Address{i}, Token1: domain.Address{i + 1}})
	}
	states = append(states, pool.State{Key: domain.PoolKey{Address: domain.Address{20}, Kind: domain.PoolKindV2}, Token0: domain.Address{1}, Token1: domain.Address{2}})
	original, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	base, err := BuildTable(original, 8, 100)
	if err != nil {
		t.Fatal(err)
	}
	oldCount := base.Len()
	oldPostings := base.Candidates([]domain.PoolKey{states[0].Key}, 100)
	closing := pool.State{Key: domain.PoolKey{Address: domain.Address{30}, Kind: domain.PoolKindV2}, Token0: domain.Address{8}, Token1: domain.Address{1}}
	states = append(states, closing)
	expanded, err := Build(states)
	if err != nil {
		t.Fatal(err)
	}
	extended, err := ExtendTableContext(t.Context(), expanded, base, []domain.PoolKey{closing.Key}, 8, 100)
	if err != nil {
		t.Fatal(err)
	}
	full, err := BuildTable(expanded, 8, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(oldPostings, base.Candidates([]domain.PoolKey{states[0].Key}, 100)) {
		t.Fatal("extension mutated old postings")
	}
	if base.Len() != oldCount || extended.Len() != full.Len() {
		t.Fatal("extension lost cycles or mutated old table", base.Len(), extended.Len(), full.Len())
	}
	keys := make(map[string]bool)
	for i := 0; i < extended.Len(); i++ {
		keys[cycleKey(extended.Cycle(i))] = true
	}
	for i := 0; i < full.Len(); i++ {
		if !keys[cycleKey(full.Cycle(i))] {
			t.Fatal("incremental and full enumeration disagree")
		}
	}
	for i := 0; i < oldCount; i++ {
		if cycleKey(base.Cycle(i)) != cycleKey(extended.Cycle(i)) {
			t.Fatal("old path identity changed")
		}
	}
	if len(base.Candidates([]domain.PoolKey{closing.Key}, 100)) != 0 || len(extended.Candidates([]domain.PoolKey{closing.Key}, 100)) != 4 {
		t.Fatal("new long routes missing from dirty index")
	}
	bounded, err := ExtendTableContext(t.Context(), expanded, base, []domain.PoolKey{closing.Key}, 8, oldCount+1)
	if !errors.Is(err, ErrTableFull) || bounded.Len() != oldCount+1 || base.Len() != oldCount {
		t.Fatal("extension did not preserve cap and immutable base", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := ExtendTableContext(ctx, expanded, base, []domain.PoolKey{closing.Key}, 8, 100); !errors.Is(err, context.Canceled) {
		t.Fatal("extension ignored cancellation", err)
	}
}

// Path enumeration/index maintenance only; the shared graph is already built.
func BenchmarkAppendOnlyPathMaintenance(b *testing.B) {
	address := func(value uint64) domain.Address {
		var a domain.Address
		binary.BigEndian.PutUint64(a[12:], value)
		return a
	}
	var states []pool.State
	var last domain.PoolKey
	for component := uint64(0); component < 500; component++ {
		for edge := uint64(0); edge < 3; edge++ {
			states = append(states, pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: address(component*3 + edge + 1)}, Token0: address(component*3 + edge + 1), Token1: address(component*3 + (edge+1)%3 + 1)})
		}
	}
	graph, err := Build(states)
	if err != nil {
		b.Fatal(err)
	}
	base, err := BuildTable(graph, 8, 10000)
	if err != nil {
		b.Fatal(err)
	}
	added := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindV2, Address: address(9000)}, Token0: states[0].Token0, Token1: states[0].Token1}
	last = added.Key
	graph, err = Build(append(states, added))
	if err != nil {
		b.Fatal(err)
	}
	for _, strategy := range []string{"full-rebuild", "added-pool"} {
		b.Run(strategy, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var table *Table
				var err error
				if strategy == "full-rebuild" {
					table, err = BuildTableContext(context.Background(), graph, 8, 10000)
				} else {
					table, err = ExtendTableContext(context.Background(), graph, base, []domain.PoolKey{last}, 8, 10000)
				}
				if err != nil || table.Len() != base.Len()+4 {
					b.Fatal("maintenance result differs", err)
				}
			}
		})
	}
}
