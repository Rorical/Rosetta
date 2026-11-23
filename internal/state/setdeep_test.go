package state

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func testDeep() pool.DeepState {
	return pool.DeepState{
		Bitmap:    map[int16]*big.Int{-1: big.NewInt(0x1000)},
		WindowMin: -1280,
		WindowMax: 1279,
		Ticks: map[int32]pool.TickInfo{
			-120: {LiquidityGross: big.NewInt(1_000), LiquidityNet: big.NewInt(1_000)},
			120:  {LiquidityGross: big.NewInt(1_000), LiquidityNet: big.NewInt(-1_000)},
		},
	}
}

func TestSetDeepStoresIsolatedCopy(t *testing.T) {
	addr := testAddress(0x51)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	input := testDeep()
	if err := store.SetDeep(v3Key(addr), input); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}

	stored, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after SetDeep", addr)
	}
	assertDeepEqual(t, stored, input)

	// Mutating the caller's input after SetDeep must not change the store.
	input.Bitmap[-1].SetInt64(0)
	input.Ticks[-120].LiquidityGross.SetInt64(0)
	delete(input.Ticks, 120)

	again, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after mutating the input", addr)
	}
	assertDeepEqual(t, again, testDeep())

	// Mutating a returned copy must not change the store either.
	again.Ticks[-120].LiquidityNet.SetInt64(0)
	again.Bitmap[-1].SetInt64(0)

	third, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after mutating a copy", addr)
	}
	assertDeepEqual(t, third, testDeep())
}

func TestSetDeepNilMapsBecomeEmpty(t *testing.T) {
	addr := testAddress(0x52)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.SetDeep(v3Key(addr), pool.DeepState{}); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}

	stored, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after SetDeep", addr)
	}
	if stored.Bitmap == nil || stored.Ticks == nil {
		t.Fatalf("SetDeep stored nil maps: bitmap %v, ticks %v", stored.Bitmap, stored.Ticks)
	}
	if len(stored.Bitmap) != 0 || len(stored.Ticks) != 0 {
		t.Fatalf("SetDeep stored non-empty maps from a zero value")
	}
}

func TestDeepStatusReportsFreshness(t *testing.T) {
	addr := testAddress(0x58)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, _, _, ok := store.DeepStatus(v3Key(addr)); ok {
		t.Fatal("DeepStatus reported deep state before SetDeep")
	}

	deep := testDeep()
	deep.LoadedBlock = blockRef(10)
	if err := store.SetDeep(v3Key(addr), deep); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}
	min, max, loaded, ok := store.DeepStatus(v3Key(addr))
	if !ok {
		t.Fatal("DeepStatus missed stored deep state")
	}
	if min != deep.WindowMin || max != deep.WindowMax || loaded != deep.LoadedBlock {
		t.Fatalf("DeepStatus = ([%d, %d], %+v), want ([%d, %d], %+v)",
			min, max, loaded, deep.WindowMin, deep.WindowMax, deep.LoadedBlock)
	}
}

func TestSetDeepDoesNotMarkDirty(t *testing.T) {
	addr := testAddress(0x53)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	store.ClearDirty()

	if err := store.SetDeep(v3Key(addr), testDeep()); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("SetDeep marked pools dirty: %v", dirty)
	}
}

func TestSetDeepUnknownPool(t *testing.T) {
	addr := testAddress(0x54)
	store := New()
	err := store.SetDeep(v3Key(addr), testDeep())
	if !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("SetDeep error = %v, want %v", err, ErrPoolNotFound)
	}
	if _, ok := store.Deep(v3Key(addr)); ok {
		t.Fatalf("SetDeep created deep state for missing pool %s", addr)
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("rejected SetDeep marked pools dirty: %v", dirty)
	}
}

func TestSetDeepRollbackRestoresPreviousDeep(t *testing.T) {
	t.Run("previous deep restored", func(t *testing.T) {
		addr := testAddress(0x55)
		store := seededStore(t, addr)
		before, ok := store.Deep(v3Key(addr))
		if !ok {
			t.Fatalf("Deep(%s) missing before SetDeep", addr)
		}
		if err := store.BeginBlock(domain.BlockRef{Number: 40, Hash: domain.Hash{0x28}}); err != nil {
			t.Fatalf("BeginBlock: %v", err)
		}

		replacement := pool.DeepState{
			Bitmap: map[int16]*big.Int{7: big.NewInt(1)},
			Ticks:  map[int32]pool.TickInfo{70: {LiquidityGross: big.NewInt(7), LiquidityNet: big.NewInt(7)}},
		}
		if err := store.SetDeep(v3Key(addr), replacement); err != nil {
			t.Fatalf("SetDeep: %v", err)
		}
		if dirty := store.Dirty(); len(dirty) != 0 {
			t.Fatalf("SetDeep in frame marked pools dirty: %v", dirty)
		}

		if err := store.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}

		after, ok := store.Deep(v3Key(addr))
		if !ok {
			t.Fatalf("Rollback removed deep state for %s", addr)
		}
		assertDeepEqual(t, after, before)
	})

	t.Run("deep created in frame removed", func(t *testing.T) {
		addr := testAddress(0x56)
		store := New()
		if err := store.Upsert(v3State(addr)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		store.ClearDirty()
		if err := store.BeginBlock(domain.BlockRef{Number: 41, Hash: domain.Hash{0x29}}); err != nil {
			t.Fatalf("BeginBlock: %v", err)
		}

		if err := store.SetDeep(v3Key(addr), testDeep()); err != nil {
			t.Fatalf("SetDeep: %v", err)
		}
		if _, ok := store.Deep(v3Key(addr)); !ok {
			t.Fatalf("Deep(%s) missing after SetDeep", addr)
		}

		if err := store.Rollback(); err != nil {
			t.Fatalf("Rollback: %v", err)
		}

		if _, ok := store.Deep(v3Key(addr)); ok {
			t.Fatalf("Rollback kept deep state created in the frame")
		}
		if _, ok := store.Pool(v3Key(addr)); !ok {
			t.Fatalf("Rollback removed shallow state that existed before the frame")
		}
		if dirty := store.Dirty(); len(dirty) != 0 {
			t.Fatalf("Rollback left pools dirty: %v", dirty)
		}
	})
}

func TestSetDeepCommitKeepsState(t *testing.T) {
	addr := testAddress(0x57)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.BeginBlock(domain.BlockRef{Number: 42, Hash: domain.Hash{0x2a}}); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}
	if err := store.SetDeep(v3Key(addr), testDeep()); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}

	store.Commit()

	if err := store.Rollback(); !errors.Is(err, ErrNoUndoFrame) {
		t.Fatalf("Rollback after Commit error = %v, want %v", err, ErrNoUndoFrame)
	}
	after, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Commit dropped deep state for %s", addr)
	}
	assertDeepEqual(t, after, testDeep())
}
