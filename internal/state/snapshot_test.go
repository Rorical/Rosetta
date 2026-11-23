package state

import (
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestSnapshotRestoreRoundTrip(t *testing.T) {
	store := New()
	head := domain.BlockRef{Number: 100, Hash: domain.Hash{0x0a}, Timestamp: 1_700_000_000, L1Block: 42}
	if err := store.BeginBlock(head); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}

	a := testAddress(0x02)
	b := testAddress(0x01)
	if err := store.Upsert(v3State(a)); err != nil {
		t.Fatalf("Upsert v3: %v", err)
	}
	if err := store.ApplyV3(v3Key(a), initializeEvent(), head.Number); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	if err := store.ApplyV3(v3Key(a), mintEvent(-120, 120, 1_000_000), head.Number); err != nil {
		t.Fatalf("ApplyV3 mint: %v", err)
	}
	if err := store.Upsert(v2State(b)); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}

	wantA, _ := store.Pool(v3Key(a))
	wantADeep, _ := store.Deep(v3Key(a))
	wantB, _ := store.Pool(v2Key(b))
	wantDirty := store.Dirty()
	snap := store.Snapshot()

	// Move the live store away from the snapshot in every dimension.
	if err := store.BeginBlock(domain.BlockRef{Number: 200}); err != nil {
		t.Fatalf("BeginBlock forward: %v", err)
	}
	if err := store.Upsert(v3State(testAddress(0x03))); err != nil {
		t.Fatalf("Upsert extra: %v", err)
	}
	if err := store.ApplyV3(v3Key(a), swapEvent(testQ96(), 5, 10), store.Block().Number); err != nil {
		t.Fatalf("ApplyV3 swap: %v", err)
	}
	store.ClearDirty()

	assertRestored := func(t *testing.T) {
		t.Helper()
		if got := store.Block(); got != head {
			t.Fatalf("Block() = %+v, want %+v", got, head)
		}
		if _, ok := store.Pool(v3Key(testAddress(0x03))); ok {
			t.Fatalf("restored store kept a pool added after the snapshot")
		}
		gotA, ok := store.Pool(v3Key(a))
		if !ok {
			t.Fatalf("Pool(%s) missing after restore", a)
		}
		assertPoolEqual(t, gotA, wantA)
		gotADeep, ok := store.Deep(v3Key(a))
		if !ok {
			t.Fatalf("Deep(%s) missing after restore", a)
		}
		assertDeepEqual(t, gotADeep, wantADeep)
		gotB, ok := store.Pool(v2Key(b))
		if !ok {
			t.Fatalf("Pool(%s) missing after restore", b)
		}
		assertPoolEqual(t, gotB, wantB)
		assertPoolKeys(t, store.Dirty(), wantDirty)
	}

	if err := store.Restore(snap); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertRestored(t)

	// A second restore after mutating the restored store proves the snapshot
	// does not share memory with the store it was restored into.
	if err := store.ApplyV3(v3Key(a), burnEvent(-120, 120, 500), store.Block().Number); err != nil {
		t.Fatalf("ApplyV3 burn: %v", err)
	}
	if err := store.Upsert(v2State(testAddress(0x04))); err != nil {
		t.Fatalf("Upsert after restore: %v", err)
	}
	store.ClearDirty()

	if err := store.Restore(snap); err != nil {
		t.Fatalf("second Restore: %v", err)
	}
	assertRestored(t)
}

func TestRestoreRejectsInvalidSnapshot(t *testing.T) {
	a := testAddress(0x01)
	b := testAddress(0x02)
	emptyPools := map[domain.PoolKey]pool.State{}
	emptyDeep := map[domain.PoolKey]pool.DeepState{}
	emptyDirty := map[domain.PoolKey]struct{}{}
	emptyStale := map[domain.PoolKey]struct{}{}

	tests := []struct {
		name string
		snap Snapshot
	}{
		{
			name: "nil pools",
			snap: Snapshot{deep: emptyDeep, dirty: emptyDirty, deepStale: emptyStale},
		},
		{
			name: "nil deep",
			snap: Snapshot{pools: map[domain.PoolKey]pool.State{v3Key(a): v3State(a)}, dirty: emptyDirty, deepStale: emptyStale},
		},
		{
			name: "nil dirty",
			snap: Snapshot{pools: map[domain.PoolKey]pool.State{v3Key(a): v3State(a)}, deep: emptyDeep, deepStale: emptyStale},
		},
		{
			name: "nil stale",
			snap: Snapshot{pools: map[domain.PoolKey]pool.State{v3Key(a): v3State(a)}, deep: emptyDeep, dirty: emptyDirty},
		},
		{
			name: "zero pool address",
			snap: Snapshot{
				pools:     map[domain.PoolKey]pool.State{{}: v3State(domain.Address{})},
				deep:      emptyDeep,
				dirty:     emptyDirty,
				deepStale: emptyStale,
			},
		},
		{
			name: "pool key mismatch",
			snap: Snapshot{
				pools:     map[domain.PoolKey]pool.State{v3Key(a): v3State(b)},
				deep:      emptyDeep,
				dirty:     emptyDirty,
				deepStale: emptyStale,
			},
		},
		{
			name: "deep state without pool",
			snap: Snapshot{
				pools:     emptyPools,
				deep:      map[domain.PoolKey]pool.DeepState{v3Key(a): {}},
				dirty:     emptyDirty,
				deepStale: emptyStale,
			},
		},
		{
			name: "dirty marker without pool",
			snap: Snapshot{
				pools:     emptyPools,
				deep:      emptyDeep,
				dirty:     map[domain.PoolKey]struct{}{v3Key(a): {}},
				deepStale: emptyStale,
			},
		},
		{
			name: "stale marker without pool",
			snap: Snapshot{
				pools:     emptyPools,
				deep:      emptyDeep,
				dirty:     emptyDirty,
				deepStale: map[domain.PoolKey]struct{}{v3Key(a): {}},
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			head := domain.BlockRef{Number: 5, Hash: domain.Hash{0x09}}
			if err := store.BeginBlock(head); err != nil {
				t.Fatalf("BeginBlock: %v", err)
			}
			existing := testAddress(0x09)
			if err := store.Upsert(v3State(existing)); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			wantPool, _ := store.Pool(v3Key(existing))

			err := store.Restore(test.snap)
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("Restore error = %v, want %v", err, ErrInvalidSnapshot)
			}
			if got := store.Block(); got != head {
				t.Fatalf("invalid Restore changed block to %+v", got)
			}
			gotPool, ok := store.Pool(v3Key(existing))
			if !ok {
				t.Fatalf("invalid Restore removed pool %s", existing)
			}
			assertPoolEqual(t, gotPool, wantPool)
			assertPoolKeys(t, store.Dirty(), []domain.PoolKey{v3Key(existing)})
		})
	}
}
