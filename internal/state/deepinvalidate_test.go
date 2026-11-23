package state

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// deepAt returns deep state stamped with the given loaded block.
func deepAt(number uint64) pool.DeepState {
	return pool.DeepState{LoadedBlock: domain.BlockRef{Number: number, Hash: domain.Hash{byte(number)}}}
}

func TestRollbackToDropsDeepLoadedAboveTarget(t *testing.T) {
	tests := []struct {
		name       string
		loaded     uint64
		wantExists bool
	}{
		{name: "above target is dropped", loaded: 10, wantExists: false},
		{name: "at target survives", loaded: 9, wantExists: true},
		{name: "zero number is dropped as unknown", loaded: 0, wantExists: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			addr := testAddress(0x80)
			if err := store.Upsert(v3State(addr)); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			beginBlock(t, store, 9)
			store.Commit()
			beginBlock(t, store, 10)
			store.Commit()
			// A refresh after Commit writes deep state outside the frame.
			if err := store.SetDeep(v3Key(addr), deepAt(test.loaded)); err != nil {
				t.Fatalf("SetDeep: %v", err)
			}

			if err := store.RollbackTo(9); err != nil {
				t.Fatalf("RollbackTo(9): %v", err)
			}
			if got := store.Block().Number; got != 9 {
				t.Fatalf("head = %d, want 9", got)
			}
			_, ok := store.Deep(v3Key(addr))
			if ok != test.wantExists {
				t.Fatalf("Deep(%s) present = %v, want %v", addr, ok, test.wantExists)
			}
		})
	}
}

func TestRollbackToAcrossTwoFramesDropsNewestDeep(t *testing.T) {
	store := New()
	older := testAddress(0x81)
	newer := testAddress(0x82)
	for _, addr := range []domain.Address{older, newer} {
		if err := store.Upsert(v3State(addr)); err != nil {
			t.Fatalf("Upsert(%s): %v", addr, err)
		}
	}
	beginBlock(t, store, 9)
	store.Commit()
	beginBlock(t, store, 10)
	store.Commit()
	beginBlock(t, store, 11)
	store.Commit()

	if err := store.SetDeep(v3Key(older), deepAt(9)); err != nil {
		t.Fatalf("SetDeep(older): %v", err)
	}
	if err := store.SetDeep(v3Key(newer), deepAt(10)); err != nil {
		t.Fatalf("SetDeep(newer): %v", err)
	}

	if err := store.RollbackTo(9); err != nil {
		t.Fatalf("RollbackTo(9): %v", err)
	}
	if got := store.Block().Number; got != 9 {
		t.Fatalf("head = %d, want 9", got)
	}
	if _, ok := store.Deep(v3Key(older)); !ok {
		t.Fatalf("deep state loaded at the target block was dropped")
	}
	if _, ok := store.Deep(v3Key(newer)); ok {
		t.Fatalf("deep state loaded above the target block survived")
	}
}

func TestRollbackDropsDeepLoadedAboveRestoredHead(t *testing.T) {
	tests := []struct {
		name       string
		loaded     uint64
		wantExists bool
	}{
		{name: "above head is dropped", loaded: 10, wantExists: false},
		{name: "at head survives", loaded: 9, wantExists: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			addr := testAddress(0x83)
			if err := store.Upsert(v3State(addr)); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			beginBlock(t, store, 9)
			store.Commit()
			if err := store.SetDeep(v3Key(addr), deepAt(test.loaded)); err != nil {
				t.Fatalf("SetDeep: %v", err)
			}
			beginBlock(t, store, 10)

			if err := store.Rollback(); err != nil {
				t.Fatalf("Rollback: %v", err)
			}
			if got := store.Block().Number; got != 9 {
				t.Fatalf("head = %d, want 9", got)
			}
			_, ok := store.Deep(v3Key(addr))
			if ok != test.wantExists {
				t.Fatalf("Deep(%s) present = %v, want %v", addr, ok, test.wantExists)
			}
		})
	}
}
