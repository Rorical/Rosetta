package state

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestRollbackRemovesPoolCreatedInFrame(t *testing.T) {
	store := New()
	prev := domain.BlockRef{Number: 9, Hash: domain.Hash{0x09}, Timestamp: 900, L1Block: 4}
	head := domain.BlockRef{Number: 10, Hash: domain.Hash{0x0a}, Timestamp: 1_000, L1Block: 5}
	if err := store.BeginBlock(prev); err != nil {
		t.Fatalf("BeginBlock(prev): %v", err)
	}
	store.Commit()

	if err := store.BeginBlock(head); err != nil {
		t.Fatalf("BeginBlock(head): %v", err)
	}
	addr := testAddress(0x41)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.ApplyV3(v3Key(addr), initializeEvent(), head.Number); err != nil {
		t.Fatalf("ApplyV3: %v", err)
	}

	if err := store.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := store.Block(); got != prev {
		t.Fatalf("Block() = %+v, want %+v", got, prev)
	}
	if _, ok := store.Pool(v3Key(addr)); ok {
		t.Fatalf("Rollback kept pool %s created in the frame", addr)
	}
	if _, ok := store.Deep(v3Key(addr)); ok {
		t.Fatalf("Rollback kept deep state %s created in the frame", addr)
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("Dirty() = %v after rollback, want empty", dirty)
	}
}

func TestRollbackRestoresExistingPool(t *testing.T) {
	t.Run("shallow refresh and event", func(t *testing.T) {
		addr := testAddress(0x42)
		store := seededStore(t, addr)
		if err := store.BeginBlock(domain.BlockRef{Number: 20, Hash: domain.Hash{0x14}}); err != nil {
			t.Fatalf("BeginBlock: %v", err)
		}

		before, ok := store.Pool(v3Key(addr))
		if !ok {
			t.Fatalf("Pool(%s) missing before mutation", addr)
		}
		beforeDeep, ok := store.Deep(v3Key(addr))
		if !ok {
			t.Fatalf("Deep(%s) missing before mutation", addr)
		}

		refreshed := v3State(addr)
		refreshed.SqrtPriceX96 = new(big.Int).Lsh(big.NewInt(2), 96)
		refreshed.Liquidity = big.NewInt(500_000)
		refreshed.Tick = -7
		refreshed.UpdatedBlock = 20
		if err := store.Upsert(refreshed); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if err := store.ApplyV3(v3Key(addr), swapEvent(new(big.Int).Lsh(big.NewInt(3), 95), 12, 250_000), 20); err != nil {
			t.Fatalf("ApplyV3: %v", err)
		}
		assertPoolKeys(t, store.Dirty(), []domain.PoolKey{v3Key(addr)})

		if err := store.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}

		after, ok := store.Pool(v3Key(addr))
		if !ok {
			t.Fatalf("Rollback removed pool %s", addr)
		}
		assertPoolEqual(t, after, before)

		afterDeep, ok := store.Deep(v3Key(addr))
		if !ok {
			t.Fatalf("Rollback removed deep state for %s", addr)
		}
		assertDeepEqual(t, afterDeep, beforeDeep)

		if dirty := store.Dirty(); len(dirty) != 0 {
			t.Fatalf("Dirty() = %v after rollback, want empty", dirty)
		}
	})

	t.Run("identity change that drops deep", func(t *testing.T) {
		addr := testAddress(0x43)
		store := seededStore(t, addr)
		before, ok := store.Pool(v3Key(addr))
		if !ok {
			t.Fatalf("Pool(%s) missing before mutation", addr)
		}
		beforeDeep, ok := store.Deep(v3Key(addr))
		if !ok {
			t.Fatalf("Deep(%s) missing before mutation", addr)
		}
		if err := store.BeginBlock(domain.BlockRef{Number: 21, Hash: domain.Hash{0x15}}); err != nil {
			t.Fatalf("BeginBlock: %v", err)
		}

		if err := store.Upsert(v2State(addr)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		if _, ok := store.Deep(v3Key(addr)); ok {
			t.Fatalf("Upsert kept deep state after identity change")
		}

		if err := store.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		after, ok := store.Pool(v3Key(addr))
		if !ok {
			t.Fatalf("Rollback removed pool %s", addr)
		}
		assertPoolEqual(t, after, before)
		afterDeep, ok := store.Deep(v3Key(addr))
		if !ok {
			t.Fatalf("Rollback did not restore deep state for %s", addr)
		}
		assertDeepEqual(t, afterDeep, beforeDeep)
	})

	t.Run("dirty membership from frame start", func(t *testing.T) {
		addr := testAddress(0x4b)
		store := New()
		if err := store.Upsert(v3State(addr)); err != nil {
			t.Fatalf("Upsert before frame: %v", err)
		}
		if err := store.BeginBlock(domain.BlockRef{Number: 22, Hash: domain.Hash{0x16}}); err != nil {
			t.Fatalf("BeginBlock: %v", err)
		}
		updated := v3State(addr)
		updated.Tick = 4
		if err := store.Upsert(updated); err != nil {
			t.Fatalf("Upsert in frame: %v", err)
		}

		if err := store.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		assertPoolKeys(t, store.Dirty(), []domain.PoolKey{v3Key(addr)})
	})
}

func TestCommitKeepsChanges(t *testing.T) {
	store := New()
	head := domain.BlockRef{Number: 10, Hash: domain.Hash{0x0a}}
	if err := store.BeginBlock(head); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}
	addr := testAddress(0x44)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	before, _ := store.Pool(v3Key(addr))

	store.Commit()

	if err := store.Rollback(); !errors.Is(err, ErrNoUndoFrame) {
		t.Fatalf("Rollback after Commit error = %v, want %v", err, ErrNoUndoFrame)
	}
	if got := store.Block(); got != head {
		t.Fatalf("Block() = %+v after failed rollback, want %+v", got, head)
	}
	after, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Rollback after Commit removed pool %s", addr)
	}
	assertPoolEqual(t, after, before)
}

func TestBeginBlockCommitsOpenFrame(t *testing.T) {
	store := New()
	first := domain.BlockRef{Number: 10, Hash: domain.Hash{0x0a}, Timestamp: 1_000, L1Block: 5}
	second := domain.BlockRef{Number: 11, Hash: domain.Hash{0x0b}, Timestamp: 1_100, L1Block: 6}
	if err := store.BeginBlock(first); err != nil {
		t.Fatalf("BeginBlock(first): %v", err)
	}
	committed := testAddress(0x45)
	if err := store.Upsert(v3State(committed)); err != nil {
		t.Fatalf("Upsert committed: %v", err)
	}

	if err := store.BeginBlock(second); err != nil {
		t.Fatalf("BeginBlock(second): %v", err)
	}
	pending := testAddress(0x46)
	if err := store.Upsert(v3State(pending)); err != nil {
		t.Fatalf("Upsert pending: %v", err)
	}

	if err := store.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	if got := store.Block(); got != first {
		t.Fatalf("Block() = %+v, want %+v", got, first)
	}
	if _, ok := store.Pool(v3Key(committed)); !ok {
		t.Fatalf("BeginBlock discarded changes from the previous block")
	}
	if _, ok := store.Pool(v3Key(pending)); ok {
		t.Fatalf("Rollback kept pool %s from the new frame", pending)
	}
	assertPoolKeys(t, store.Dirty(), []domain.PoolKey{v3Key(committed)})
}

func TestRollbackWithoutFrame(t *testing.T) {
	tests := []struct {
		name  string
		build func(t *testing.T) *Store
	}{
		{
			name:  "new store",
			build: func(t *testing.T) *Store { return New() },
		},
		{
			name:  "zero store",
			build: func(t *testing.T) *Store { return new(Store) },
		},
		{
			name: "after commit",
			build: func(t *testing.T) *Store {
				store := New()
				if err := store.BeginBlock(domain.BlockRef{Number: 3}); err != nil {
					t.Fatalf("BeginBlock: %v", err)
				}
				store.Commit()
				return store
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.build(t).Rollback()
			if !errors.Is(err, ErrNoUndoFrame) {
				t.Fatalf("Rollback error = %v, want %v", err, ErrNoUndoFrame)
			}
		})
	}
}

func TestSnapshotAndRestoreClearOpenFrame(t *testing.T) {
	t.Run("snapshot", func(t *testing.T) {
		store := New()
		if err := store.BeginBlock(domain.BlockRef{Number: 10}); err != nil {
			t.Fatalf("BeginBlock: %v", err)
		}
		addr := testAddress(0x47)
		if err := store.Upsert(v3State(addr)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}

		store.Snapshot()

		if err := store.Rollback(); !errors.Is(err, ErrNoUndoFrame) {
			t.Fatalf("Rollback after Snapshot error = %v, want %v", err, ErrNoUndoFrame)
		}
		if _, ok := store.Pool(v3Key(addr)); !ok {
			t.Fatalf("Rollback after Snapshot removed pool %s", addr)
		}
	})

	t.Run("restore", func(t *testing.T) {
		store := New()
		head := domain.BlockRef{Number: 10, Hash: domain.Hash{0x0a}}
		if err := store.BeginBlock(head); err != nil {
			t.Fatalf("BeginBlock(head): %v", err)
		}
		kept := testAddress(0x48)
		if err := store.Upsert(v3State(kept)); err != nil {
			t.Fatalf("Upsert kept: %v", err)
		}
		snap := store.Snapshot()

		if err := store.BeginBlock(domain.BlockRef{Number: 11}); err != nil {
			t.Fatalf("BeginBlock(next): %v", err)
		}
		dropped := testAddress(0x49)
		if err := store.Upsert(v3State(dropped)); err != nil {
			t.Fatalf("Upsert dropped: %v", err)
		}
		if err := store.Restore(snap); err != nil {
			t.Fatalf("Restore: %v", err)
		}

		if err := store.Rollback(); !errors.Is(err, ErrNoUndoFrame) {
			t.Fatalf("Rollback after Restore error = %v, want %v", err, ErrNoUndoFrame)
		}
		if got := store.Block(); got != head {
			t.Fatalf("Block() = %+v after restore, want %+v", got, head)
		}
		if _, ok := store.Pool(v3Key(dropped)); ok {
			t.Fatalf("Restore kept pool %s added after the snapshot", dropped)
		}
		if _, ok := store.Pool(v3Key(kept)); !ok {
			t.Fatalf("Restore removed pool %s from the snapshot", kept)
		}
	})
}

func TestRollbackRestoresFrameStartValue(t *testing.T) {
	addr := testAddress(0x4a)
	store := seededStore(t, addr)
	before, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing before mutation", addr)
	}
	beforeDeep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing before mutation", addr)
	}
	if err := store.BeginBlock(domain.BlockRef{Number: 30, Hash: domain.Hash{0x1e}}); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}

	first := v3State(addr)
	first.Tick = 3
	first.Liquidity = big.NewInt(3_000)
	first.UpdatedBlock = 30
	if err := store.Upsert(first); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	if err := store.ApplyV3(v3Key(addr), swapEvent(new(big.Int).Lsh(big.NewInt(3), 95), 9, 9_000), 30); err != nil {
		t.Fatalf("ApplyV3: %v", err)
	}
	if err := store.Upsert(v2State(addr)); err != nil {
		t.Fatalf("Upsert identity change: %v", err)
	}

	if err := store.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}

	after, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Rollback removed pool %s", addr)
	}
	assertPoolEqual(t, after, before)
	afterDeep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Rollback did not restore deep state for %s", addr)
	}
	assertDeepEqual(t, afterDeep, beforeDeep)
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("Dirty() = %v after rollback, want empty", dirty)
	}
}
