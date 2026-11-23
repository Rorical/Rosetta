package state

import (
	"errors"
	"slices"
	"testing"
)

func TestCheckpointPreservesUndoHistory(t *testing.T) {
	tests := []struct {
		name    string
		capture func(*Store) error
	}{
		{
			name: "checkpoint",
			capture: func(store *Store) error {
				_ = store.Checkpoint()
				return nil
			},
		},
		{
			name: "encode snapshot",
			capture: func(store *Store) error {
				_, err := store.EncodeSnapshot()
				return err
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			addr := testAddress(0x50)
			beginBlock(t, store, 10)
			upsertTick(t, store, addr, 1, 10)
			beginBlock(t, store, 11)
			upsertTick(t, store, addr, 2, 11)
			before := store.UndoRefs()
			if len(before) < 2 {
				t.Fatalf("UndoRefs before capture = %v, want at least two refs", before)
			}

			if err := test.capture(store); err != nil {
				t.Fatalf("capture: %v", err)
			}
			if after := store.UndoRefs(); !slices.Equal(after, before) {
				t.Fatalf("UndoRefs after capture = %v, want %v", after, before)
			}
			if err := store.RollbackTo(10); err != nil {
				t.Fatalf("RollbackTo(10): %v", err)
			}
			pool, ok := store.Pool(v3Key(addr))
			if !ok || pool.Tick != 1 {
				t.Fatalf("pool after rollback = %+v, present %v; want tick 1", pool, ok)
			}
		})
	}
}

func TestSnapshotStillClearsUndoHistory(t *testing.T) {
	store := New()
	addr := testAddress(0x51)
	beginBlock(t, store, 10)
	upsertTick(t, store, addr, 1, 10)
	beginBlock(t, store, 11)
	upsertTick(t, store, addr, 2, 11)

	_ = store.Snapshot()
	if refs := store.UndoRefs(); len(refs) != 1 {
		t.Fatalf("UndoRefs after Snapshot = %v, want only the head", refs)
	}
	if err := store.RollbackTo(10); !errors.Is(err, ErrNoUndoFrame) {
		t.Fatalf("RollbackTo after Snapshot = %v, want ErrNoUndoFrame", err)
	}
}
