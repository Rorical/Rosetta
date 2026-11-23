package state

import (
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestRemoveDropsThePoolAndIsUndone(t *testing.T) {
	store := New()
	kept, removed := v3State(testAddress(0x05)), v3State(testAddress(0x20))
	for _, p := range []pool.State{kept, removed} {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.SetDeep(removed.Key, pool.DeepState{}); err != nil {
		t.Fatal(err)
	}
	store.Remove(v3Key(testAddress(0x99))) // unknown: nothing happens
	if err := store.BeginBlock(domain.BlockRef{Number: 1, Hash: domain.Hash{1}}); err != nil {
		t.Fatal(err)
	}
	store.Remove(removed.Key)
	if _, ok := store.Pool(removed.Key); ok {
		t.Fatal("pool still stored")
	}
	if _, ok := store.Deep(removed.Key); ok {
		t.Fatal("deep state still stored")
	}
	assertPoolKeys(t, store.Dirty(), []domain.PoolKey{kept.Key})
	if store.Len() != 1 {
		t.Fatalf("%d pools", store.Len())
	}
	if _, err := store.EncodeSnapshot(); err != nil {
		t.Fatalf("store inconsistent after Remove: %v", err)
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.Pool(removed.Key); !ok {
		t.Fatal("rollback did not restore the pool")
	}
	// Rollback drops deep state without a load block, restored or not.
	assertPoolKeys(t, store.Dirty(), []domain.PoolKey{kept.Key, removed.Key})
}

// The wrap of the native token has no state beyond its identity; a snapshot
// carries it like any pool.
func TestSnapshotCodecKeepsAWrapPool(t *testing.T) {
	wrapped := testAddress(0x42)
	wrap := pool.State{Key: domain.PoolKey{Address: wrapped, Kind: domain.PoolKindWrap}, Token1: wrapped}
	store := New()
	for _, p := range []pool.State{v3State(testAddress(0x05)), wrap} {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	store.ClearDirty()
	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	loaded := New()
	if err := loaded.LoadSnapshot(data); err != nil {
		t.Fatal(err)
	}
	got, ok := loaded.Pool(wrap.Key)
	if !ok || got.Token0 != (domain.Address{}) || got.Token1 != wrapped || got.Fee != 0 || got.SqrtPriceX96 != nil || got.Reserve0 != nil {
		t.Fatalf("wrap pool after the round trip: %+v, %v", got, ok)
	}
	// It needs no ticks, so it is never waiting for them.
	if len(loaded.Dirty()) != 0 || slices.Contains(loaded.DeepStale(), wrap.Key) {
		t.Fatalf("markers after the round trip: %v, %v", loaded.Dirty(), loaded.DeepStale())
	}
}
