package state

import (
	"errors"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// blockRef returns a distinct ref per block number so head comparisons catch a
// rollback that restores the wrong block.
func blockRef(number uint64) domain.BlockRef {
	return domain.BlockRef{Number: number, Hash: domain.Hash{byte(number)}}
}

func beginBlock(t *testing.T, store *Store, number uint64) {
	t.Helper()
	if err := store.BeginBlock(blockRef(number)); err != nil {
		t.Fatalf("BeginBlock(%d): %v", number, err)
	}
}

func upsertTick(t *testing.T, store *Store, addr domain.Address, tick int32, block uint64) {
	t.Helper()
	p := v3State(addr)
	p.Tick = tick
	p.UpdatedBlock = block
	if err := store.Upsert(p); err != nil {
		t.Fatalf("Upsert(%s, tick %d): %v", addr, tick, err)
	}
}

func TestRollbackToUndoesTwoBlocks(t *testing.T) {
	store := New()
	a := testAddress(0x70)
	b := testAddress(0x71)

	beginBlock(t, store, 10)
	upsertTick(t, store, a, 1, 10)

	beginBlock(t, store, 11)
	upsertTick(t, store, a, 2, 11)
	upsertTick(t, store, b, 20, 11)

	if err := store.RollbackTo(10); err != nil {
		t.Fatalf("RollbackTo(10): %v", err)
	}

	if got := store.Block(); got != blockRef(10) {
		t.Fatalf("Block() = %+v, want %+v", got, blockRef(10))
	}
	poolA, ok := store.Pool(v3Key(a))
	if !ok {
		t.Fatalf("Pool(%s) missing after rollback", a)
	}
	if poolA.Tick != 1 || poolA.UpdatedBlock != 10 {
		t.Fatalf("pool %s = tick %d block %d, want tick 1 block 10", a, poolA.Tick, poolA.UpdatedBlock)
	}
	if _, ok := store.Pool(v3Key(b)); ok {
		t.Fatalf("RollbackTo kept pool %s created in block 11", b)
	}
	assertPoolKeys(t, store.Dirty(), []domain.PoolKey{v3Key(a)})
}

func TestRollbackToUndoesThreeBlocks(t *testing.T) {
	store := New()
	addr := testAddress(0x72)

	for number := uint64(10); number <= 13; number++ {
		beginBlock(t, store, number)
		upsertTick(t, store, addr, int32(number), number)
	}

	if err := store.RollbackTo(10); err != nil {
		t.Fatalf("RollbackTo(10): %v", err)
	}

	if got := store.Block(); got != blockRef(10) {
		t.Fatalf("Block() = %+v, want %+v", got, blockRef(10))
	}
	pool, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after rollback", addr)
	}
	if pool.Tick != 10 || pool.UpdatedBlock != 10 {
		t.Fatalf("pool = tick %d block %d, want tick 10 block 10", pool.Tick, pool.UpdatedBlock)
	}
}

func TestRollbackToTargets(t *testing.T) {
	tests := []struct {
		name       string
		target     uint64
		wantNumber uint64
		wantTick   int32
	}{
		{name: "head is a no-op", target: 12, wantNumber: 12, wantTick: 12},
		{name: "previous block", target: 11, wantNumber: 11, wantTick: 11},
		{name: "older block", target: 10, wantNumber: 10, wantTick: 10},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			addr := testAddress(0x73)
			for number := uint64(10); number <= 12; number++ {
				beginBlock(t, store, number)
				upsertTick(t, store, addr, int32(number), number)
			}

			if err := store.RollbackTo(test.target); err != nil {
				t.Fatalf("RollbackTo(%d): %v", test.target, err)
			}
			if got := store.Block().Number; got != test.wantNumber {
				t.Fatalf("Block().Number = %d, want %d", got, test.wantNumber)
			}
			pool, ok := store.Pool(v3Key(addr))
			if !ok {
				t.Fatalf("Pool(%s) missing after rollback", addr)
			}
			if pool.Tick != test.wantTick {
				t.Fatalf("tick = %d, want %d", pool.Tick, test.wantTick)
			}
		})
	}
}

func TestRollbackToBoundsHistory(t *testing.T) {
	store := New()
	addr := testAddress(0x74)
	for number := uint64(10); number <= 20; number++ {
		beginBlock(t, store, number)
		upsertTick(t, store, addr, int32(number), number)
	}

	// Blocks 10 through 19 were committed into the history and trimmed to the
	// newest maxUndoBlocks frames, so the oldest reachable point is block 11.
	err := store.RollbackTo(10)
	if !errors.Is(err, ErrNoUndoFrame) {
		t.Fatalf("RollbackTo(10) error = %v, want %v", err, ErrNoUndoFrame)
	}
	if got := store.Block().Number; got != 20 {
		t.Fatalf("failed rollback moved head to %d, want 20", got)
	}
	pool, _ := store.Pool(v3Key(addr))
	if pool.Tick != 20 {
		t.Fatalf("failed rollback changed tick to %d, want 20", pool.Tick)
	}

	if err := store.RollbackTo(11); err != nil {
		t.Fatalf("RollbackTo(11): %v", err)
	}
	if got := store.Block().Number; got != 11 {
		t.Fatalf("Block().Number = %d, want 11", got)
	}
	pool, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after rollback", addr)
	}
	if pool.Tick != 11 || pool.UpdatedBlock != 11 {
		t.Fatalf("pool = tick %d block %d, want tick 11 block 11", pool.Tick, pool.UpdatedBlock)
	}
}

func TestRollbackToWithoutFrames(t *testing.T) {
	store := New()
	beginBlock(t, store, 10)
	// A snapshot clears the open frame and the undo history, leaving the head
	// with no rollback point at all.
	store.Snapshot()
	before := store.Block()

	err := store.RollbackTo(9)
	if !errors.Is(err, ErrNoUndoFrame) {
		t.Fatalf("RollbackTo error = %v, want %v", err, ErrNoUndoFrame)
	}
	if got := store.Block(); got != before {
		t.Fatalf("failed RollbackTo moved head to %+v, want %+v", got, before)
	}
}

func TestRollbackToAtOrBelowHeadIsNoop(t *testing.T) {
	store := New()
	beginBlock(t, store, 10)
	addr := testAddress(0x76)
	upsertTick(t, store, addr, 1, 10)

	if err := store.RollbackTo(10); err != nil {
		t.Fatalf("RollbackTo(head): %v", err)
	}
	if err := store.RollbackTo(11); err != nil {
		t.Fatalf("RollbackTo(above head): %v", err)
	}
	if got := store.Block(); got != blockRef(10) {
		t.Fatalf("Block() = %+v, want %+v", got, blockRef(10))
	}
	pool, ok := store.Pool(v3Key(addr))
	if !ok || pool.Tick != 1 {
		t.Fatalf("no-op RollbackTo changed pool")
	}
}

func TestCommitKeepsBlockUndoable(t *testing.T) {
	store := New()
	addr := testAddress(0x75)
	beginBlock(t, store, 10)
	upsertTick(t, store, addr, 1, 10)
	beginBlock(t, store, 11)
	upsertTick(t, store, addr, 2, 11)

	store.Commit()

	// Commit moves block 11's frame into the bounded history, so a reorg can
	// still rewind the committed block to the state block 10 left behind.
	if err := store.RollbackTo(10); err != nil {
		t.Fatalf("RollbackTo(10) after Commit: %v", err)
	}
	if got := store.Block(); got != blockRef(10) {
		t.Fatalf("Block() = %+v, want %+v", got, blockRef(10))
	}
	pool, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after rollback", addr)
	}
	if pool.Tick != 1 || pool.UpdatedBlock != 10 {
		t.Fatalf("pool = tick %d block %d, want tick 1 block 10", pool.Tick, pool.UpdatedBlock)
	}
}

func TestCommitBoundsUndoHistory(t *testing.T) {
	store := New()
	addr := testAddress(0x77)
	for number := uint64(10); number < 22; number++ {
		beginBlock(t, store, number)
		upsertTick(t, store, addr, int32(number), number)
		store.Commit()
	}

	// Twelve committed blocks leave only the newest maxUndoBlocks frames, so
	// the oldest reachable point is block 13.
	err := store.RollbackTo(12)
	if !errors.Is(err, ErrNoUndoFrame) {
		t.Fatalf("RollbackTo(12) error = %v, want %v", err, ErrNoUndoFrame)
	}
	if got := store.Block().Number; got != 21 {
		t.Fatalf("failed rollback moved head to %d, want 21", got)
	}
	pool, _ := store.Pool(v3Key(addr))
	if pool.Tick != 21 {
		t.Fatalf("failed rollback changed tick to %d, want 21", pool.Tick)
	}

	if err := store.RollbackTo(13); err != nil {
		t.Fatalf("RollbackTo(13): %v", err)
	}
	if got := store.Block().Number; got != 13 {
		t.Fatalf("Block().Number = %d, want 13", got)
	}
	pool, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after rollback", addr)
	}
	if pool.Tick != 13 || pool.UpdatedBlock != 13 {
		t.Fatalf("pool = tick %d block %d, want tick 13 block 13", pool.Tick, pool.UpdatedBlock)
	}
}
