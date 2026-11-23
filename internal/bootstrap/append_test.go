package bootstrap

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

// appendFixture seeds one V2 pool at ref and returns the store, caller, and
// the addresses of the seeded and appended pools.
func appendFixture(t *testing.T, ref domain.BlockRef) (*state.Store, *fakeCaller, domain.Address, domain.Address) {
	t.Helper()
	seeded := domain.Address{0xa1}
	added := domain.Address{0xa2}

	store := state.New()
	caller := newFakeCaller()
	caller.setOut(seeded, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(1_000, 2_000))
	if err := Seed(context.Background(), caller, store, []Spec{v2Spec(seeded)}, ref, 1); err != nil {
		t.Fatalf("Seed: %v", err)
	}
	caller.setOut(added, selectorBytes(t, uniswapv2.SelectorGetReserves), reservesOut(3_000, 4_000))
	return store, caller, seeded, added
}

func TestAppendAddsPoolAtCurrentHead(t *testing.T) {
	ref := testBlockRef(42)
	store, caller, seeded, added := appendFixture(t, ref)
	callsBefore := len(caller.calls)

	if err := Append(context.Background(), caller, store, []Spec{v2Spec(added)}, ref, 1); err != nil {
		t.Fatalf("Append: %v", err)
	}

	if got := store.Block(); got != ref {
		t.Fatalf("head = %+v, want %+v", got, ref)
	}
	for _, addr := range []domain.Address{seeded, added} {
		if _, ok := store.Pool(v2Key(addr)); !ok {
			t.Fatalf("pool %s missing after Append", addr)
		}
	}
	if len(caller.calls) != callsBefore+1 {
		t.Fatalf("calls = %d, want %d", len(caller.calls), callsBefore+1)
	}
	if last := caller.calls[len(caller.calls)-1]; last.to != added || last.block != "0x2a" {
		t.Fatalf("last call = %+v, want %s at block 0x2a", last, added)
	}
}

func TestAppendSkipsStoredKeys(t *testing.T) {
	ref := testBlockRef(42)
	store, caller, seeded, _ := appendFixture(t, ref)
	callsBefore := len(caller.calls)

	if err := Append(context.Background(), caller, store, []Spec{v2Spec(seeded)}, ref, 1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if len(caller.calls) != callsBefore {
		t.Fatalf("calls = %d, want %d: a stored key was loaded again", len(caller.calls), callsBefore)
	}
	if got := store.Block(); got != ref {
		t.Fatalf("head = %+v, want %+v", got, ref)
	}
}

func TestAppendRejectsWrongHead(t *testing.T) {
	ref := testBlockRef(42)
	store, caller, _, added := appendFixture(t, ref)

	tests := []struct {
		name string
		ref  domain.BlockRef
	}{
		{"different number", domain.BlockRef{Number: ref.Number + 1, Hash: ref.Hash}},
		{"different hash", domain.BlockRef{Number: ref.Number, Hash: domain.Hash{0xff}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			callsBefore := len(caller.calls)
			err := Append(context.Background(), caller, store, []Spec{v2Spec(added)}, test.ref, 1)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want ErrInvalidSpec", err)
			}
			if len(caller.calls) != callsBefore {
				t.Fatalf("calls = %d, want %d", len(caller.calls), callsBefore)
			}
			if _, ok := store.Pool(v2Key(added)); ok {
				t.Fatalf("pool %s stored after a rejected append", added)
			}
		})
	}
}

func TestAppendRestoresOnFailure(t *testing.T) {
	ref := testBlockRef(42)
	store, caller, seeded, added := appendFixture(t, ref)
	// The second spec has no caller response, so its load fails after the
	// first one was already written.
	broken := domain.Address{0xb1}

	err := Append(context.Background(), caller, store, []Spec{v2Spec(added), v2Spec(broken)}, ref, 1)
	if err == nil {
		t.Fatal("Append succeeded, want a load failure")
	}
	if got := store.Block(); got != ref {
		t.Fatalf("head = %+v, want %+v", got, ref)
	}
	if _, ok := store.Pool(v2Key(added)); ok {
		t.Fatalf("pool %s survived the failed append", added)
	}
	if _, ok := store.Pool(v2Key(broken)); ok {
		t.Fatalf("pool %s survived the failed append", broken)
	}
	if _, ok := store.Pool(v2Key(seeded)); !ok {
		t.Fatalf("previously seeded pool %s was lost", seeded)
	}
}

// TestAppendIsUndoable checks that an append joins the undo history instead of
// discarding it: rolling back below the append removes the appended pool and
// restores the pre-append head, while the older frames still work.
func TestAppendIsUndoable(t *testing.T) {
	store := state.New()
	caller := newFakeCaller()
	refLow := testBlockRef(41)
	refHigh := testBlockRef(42)
	first := domain.Address{0xa1}
	second := domain.Address{0xa2}
	appended := domain.Address{0xa3}
	for i, addr := range []domain.Address{first, second, appended} {
		caller.setOut(addr, selectorBytes(t, uniswapv2.SelectorGetReserves),
			reservesOut(int64(100*(i+1)), int64(200*(i+1))))
	}

	ctx := context.Background()
	if err := Seed(ctx, caller, store, []Spec{v2Spec(first)}, refLow, 1); err != nil {
		t.Fatalf("Seed low: %v", err)
	}
	if err := Seed(ctx, caller, store, []Spec{v2Spec(second)}, refHigh, 1); err != nil {
		t.Fatalf("Seed high: %v", err)
	}
	before := store.UndoRefs()

	if err := Append(ctx, caller, store, []Spec{v2Spec(appended)}, refHigh, 1); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := store.Block(); got != refHigh {
		t.Fatalf("head after Append = %+v, want %+v", got, refHigh)
	}
	if _, ok := store.Pool(v2Key(appended)); !ok {
		t.Fatalf("appended pool %s missing", appended)
	}
	for _, ref := range before {
		if !slices.Contains(store.UndoRefs(), ref) {
			t.Fatalf("UndoRefs = %v, want the pre-existing ref %+v", store.UndoRefs(), ref)
		}
	}

	if err := store.RollbackTo(refLow.Number); err != nil {
		t.Fatalf("RollbackTo(%d): %v", refLow.Number, err)
	}
	if got := store.Block(); got != refLow {
		t.Fatalf("head after rollback = %+v, want %+v", got, refLow)
	}
	if _, ok := store.Pool(v2Key(appended)); ok {
		t.Fatalf("rollback kept the appended pool %s", appended)
	}
	if _, ok := store.Pool(v2Key(first)); !ok {
		t.Fatalf("rollback lost pool %s from the low block", first)
	}
}
