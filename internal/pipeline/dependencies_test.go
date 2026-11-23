package pipeline

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestDependencyRetentionAdoptsOnlyFreshCanonicalFees(t *testing.T) {
	for _, source := range []domain.StateSource{domain.SourceStandard, domain.SourceFast} {
		store := state.New()
		anchor := domain.BlockRef{Number: 10, Hash: domain.Hash{1}}
		if err := store.BeginBlock(anchor); err != nil {
			t.Fatal(err)
		}
		p := pool.State{Key: domain.PoolKey{Address: domain.Address{1}, Kind: domain.PoolKindV2}, Token0: domain.Address{2}, Token1: domain.Address{3}, Reserve0: big.NewInt(1000), Reserve1: big.NewInt(2000), Fee: 3000}
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
		store.Commit()
		store.ClearDirty()
		refreshed := store.View().Fork()
		p.Fee = 500
		p.Reserve0 = big.NewInt(9000)
		if err := refreshed.Upsert(p); err != nil {
			t.Fatal(err)
		}
		version := domain.StateVersion{Source: source, Anchor: anchor}
		changed, _, err := RetainDependencies(store, refreshed.View(), version, []domain.PoolKey{p.Key}, nil)
		if err != nil {
			t.Fatal(err)
		}
		current, _ := store.Pool(p.Key)
		if current.Reserve0.Int64() != 1000 {
			t.Fatal("dependency refresh overwrote logged reserves")
		}
		if source == domain.SourceFast {
			if current.Fee != 3000 || len(changed) != 0 {
				t.Fatal("Fast fee contaminated canonical state")
			}
			continue
		}
		if current.Fee != 500 || len(changed) != 1 {
			t.Fatal("fresh fee was not retained")
		}
		changed, _, err = RetainDependencies(store, refreshed.View(), version, []domain.PoolKey{p.Key}, nil)
		if err != nil || len(changed) != 0 {
			t.Fatal("identical refresh was recorded again", changed, err)
		}
		stale := version
		stale.Anchor.Hash = domain.Hash{2}
		p.Fee = 900
		if err := refreshed.Upsert(p); err != nil {
			t.Fatal(err)
		}
		if _, _, err := RetainDependencies(store, refreshed.View(), stale, []domain.PoolKey{p.Key}, nil); err != nil {
			t.Fatal(err)
		}
		current, _ = store.Pool(p.Key)
		if current.Fee != 500 {
			t.Fatal("stale snapshot overwrote current fee")
		}
	}
}
