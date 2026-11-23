package bootstrap

import (
	"context"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestAppendBatchConsumesOneUndoFrame(t *testing.T) {
	low := testBlockRef(41)
	high := testBlockRef(42)
	ctx := context.Background()
	store := state.New()
	caller := newFakeCaller()

	seeded := domain.Address{0xa0}
	caller.setOut(seeded, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(1_000, 2_000))
	if err := Seed(ctx, caller, store, []Spec{v2Spec(seeded)}, low, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	// Simulate the pipeline applying one block above the seed head.
	if err := store.BeginBlock(high); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}
	store.Commit()

	specs := make([]Spec, 8)
	for i := range specs {
		addr := domain.Address{0xb0, byte(i)}
		specs[i] = v2Spec(addr)
		caller.setOut(addr, selectorBytes(t, uniswapv2.SelectorGetReserves),
			reservesOut(int64(10*(i+1)), int64(20*(i+1))))
	}

	appended, err := AppendBatch(ctx, caller, store, specs, high, 1)
	if err != nil {
		t.Fatalf("AppendBatch: %v", err)
	}
	if len(appended) != len(specs) {
		t.Fatalf("appended = %d specs, want %d", len(appended), len(specs))
	}
	for _, spec := range specs {
		if _, ok := store.Pool(spec.key()); !ok {
			t.Fatalf("pool %s missing after the batch", spec.Address)
		}
	}

	// One frame for the whole batch keeps the block frame below it reachable.
	// Eight per-pool appends would fill the bounded history and make this fail.
	if err := store.RollbackTo(low.Number); err != nil {
		t.Fatalf("RollbackTo(%d) after the batch: %v", low.Number, err)
	}
	if got := store.Block(); got != low {
		t.Fatalf("head after rollback = %+v, want %+v", got, low)
	}
	for _, spec := range specs {
		if _, ok := store.Pool(spec.key()); ok {
			t.Fatalf("pool %s survived the rollback", spec.Address)
		}
	}
	if _, ok := store.Pool(v2Key(seeded)); !ok {
		t.Fatalf("seeded pool %s was lost", seeded)
	}
}

func TestAppendBatchFailureKeepsUndoHistory(t *testing.T) {
	low := testBlockRef(41)
	high := testBlockRef(42)
	ctx := context.Background()
	store := state.New()
	caller := newFakeCaller()

	seeded := domain.Address{0xa0}
	good := domain.Address{0xb1}
	bad := domain.Address{0xb2}
	caller.setOut(seeded, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(1_000, 2_000))
	caller.setOut(good, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(3_000, 4_000))
	if err := Seed(ctx, caller, store, []Spec{v2Spec(seeded)}, low, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	if err := store.BeginBlock(high); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}
	store.Commit()

	// The batch writes the good pool first, then fails on the bad one.
	appended, err := AppendBatch(ctx, caller, store, []Spec{v2Spec(good), v2Spec(bad)}, high, 1)
	if err == nil {
		t.Fatal("AppendBatch succeeded, want a load failure")
	}
	if appended != nil {
		t.Fatalf("appended = %v after a failed batch, want nil", appended)
	}
	if got := store.Block(); got != high {
		t.Fatalf("head after failed batch = %+v, want %+v", got, high)
	}
	for _, addr := range []domain.Address{good, bad} {
		if _, ok := store.Pool(v2Key(addr)); ok {
			t.Fatalf("pool %s survived the failed batch", addr)
		}
	}

	// Rollback, not Restore, so the block frame opened before the batch is
	// still reachable.
	if err := store.RollbackTo(low.Number); err != nil {
		t.Fatalf("RollbackTo(%d) after a failed batch: %v", low.Number, err)
	}
	if got := store.Block(); got != low {
		t.Fatalf("head after rollback = %+v, want %+v", got, low)
	}
	if _, ok := store.Pool(v2Key(seeded)); !ok {
		t.Fatalf("seeded pool %s was lost", seeded)
	}
}

func TestAppendBatchEmptyIsNoop(t *testing.T) {
	ref := testBlockRef(42)
	store, caller, _, _ := appendFixture(t, ref)
	callsBefore := len(caller.calls)

	appended, err := AppendBatch(context.Background(), caller, store, nil, ref, 1)
	if err != nil {
		t.Fatalf("AppendBatch(nil): %v", err)
	}
	if len(appended) != 0 {
		t.Fatalf("appended = %v, want none", appended)
	}
	if len(caller.calls) != callsBefore {
		t.Fatalf("calls = %d, want %d", len(caller.calls), callsBefore)
	}
	if got := store.Block(); got != ref {
		t.Fatalf("head = %+v, want %+v", got, ref)
	}
}
