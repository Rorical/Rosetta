package bootstrap

import (
	"context"
	"errors"
	"math/big"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestSetHeadRecordsHeadWithoutPools(t *testing.T) {
	store := state.New()
	ref := testBlockRef(42)

	if err := SetHead(context.Background(), store, ref); err != nil {
		t.Fatalf("SetHead: %v", err)
	}
	if got := store.Block(); got != ref {
		t.Fatalf("head = %+v, want %+v", got, ref)
	}
	if pools := store.Pools(); len(pools) != 0 {
		t.Fatalf("pools = %d, want none", len(pools))
	}
	if refs := store.UndoRefs(); !slices.Equal(refs, []domain.BlockRef{ref}) {
		t.Fatalf("UndoRefs = %v, want [%+v]", refs, ref)
	}

	// The empty head frame is a real undo frame: rolling below it clears the
	// head while there is nothing else to restore.
	if err := store.RollbackTo(0); err != nil {
		t.Fatalf("RollbackTo(0): %v", err)
	}
	if got := store.Block(); got.Number != 0 {
		t.Fatalf("head after rollback = %+v, want the zero block", got)
	}
}

func TestSetHeadRejectsNonEmptyStore(t *testing.T) {
	ref := testBlockRef(42)
	withPool := func() *state.Store {
		store := state.New()
		stored := pool.State{
			Key:      domain.PoolKey{Address: domain.Address{0xa1}, Kind: domain.PoolKindV2},
			Token0:   testToken0,
			Token1:   testToken1,
			Fee:      30,
			Reserve0: big.NewInt(1),
			Reserve1: big.NewInt(2),
		}
		if err := store.Upsert(stored); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
		return store
	}
	atOtherHead := func() *state.Store {
		store := state.New()
		if err := SetHead(context.Background(), store, testBlockRef(41)); err != nil {
			t.Fatalf("SetHead(41): %v", err)
		}
		return store
	}

	tests := []struct {
		name  string
		store *state.Store
		ref   domain.BlockRef
	}{
		{"mismatched head", atOtherHead(), ref},
		{"pools without a head", withPool(), ref},
		{"zero number", state.New(), domain.BlockRef{Hash: ref.Hash}},
		{"zero hash", state.New(), domain.BlockRef{Number: ref.Number}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			before := test.store.Block()
			err := SetHead(context.Background(), test.store, test.ref)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("SetHead error = %v, want ErrInvalidSpec", err)
			}
			if got := test.store.Block(); got != before {
				t.Fatalf("head changed to %+v on a rejected call, want %+v", got, before)
			}
		})
	}
}

func TestSetHeadAcceptsMatchingHead(t *testing.T) {
	store := state.New()
	ref := testBlockRef(42)
	if err := SetHead(context.Background(), store, ref); err != nil {
		t.Fatalf("SetHead: %v", err)
	}
	if err := SetHead(context.Background(), store, ref); err != nil {
		t.Fatalf("SetHead with the same ref: %v", err)
	}
	if got := store.Block(); got != ref {
		t.Fatalf("head = %+v, want %+v", got, ref)
	}
}

func TestSetHeadRejectsNilStore(t *testing.T) {
	err := SetHead(context.Background(), nil, testBlockRef(42))
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("SetHead error = %v, want ErrInvalidSpec", err)
	}
}
