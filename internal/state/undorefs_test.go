package state

import (
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// storedBlocks commits each ref so every block leaves a restored point behind.
func storedBlocks(t *testing.T, refs ...domain.BlockRef) *Store {
	t.Helper()
	store := New()
	for _, ref := range refs {
		if err := store.BeginBlock(ref); err != nil {
			t.Fatalf("BeginBlock(%d): %v", ref.Number, err)
		}
		store.Commit()
	}
	return store
}

func ref(number uint64, hashByte byte) domain.BlockRef {
	return domain.BlockRef{Number: number, Hash: domain.Hash{hashByte}}
}

func TestUndoRefs(t *testing.T) {
	t.Run("head and two committed frames", func(t *testing.T) {
		eight, nine := ref(8, 0x08), ref(9, 0x09)
		ten := ref(10, 0x0a)
		store := storedBlocks(t, eight, nine, ten)

		want := []domain.BlockRef{ten, nine, eight}
		if got := store.UndoRefs(); !slices.Equal(got, want) {
			t.Fatalf("UndoRefs() = %+v, want %+v", got, want)
		}
	})

	t.Run("open frame is included before history", func(t *testing.T) {
		eight := ref(8, 0x08)
		nine := ref(9, 0x09)
		store := storedBlocks(t, eight)
		if err := store.BeginBlock(nine); err != nil {
			t.Fatalf("BeginBlock(9): %v", err)
		}

		want := []domain.BlockRef{nine, eight}
		if got := store.UndoRefs(); !slices.Equal(got, want) {
			t.Fatalf("UndoRefs() = %+v, want %+v", got, want)
		}
	})

	t.Run("bounded history", func(t *testing.T) {
		refs := make([]domain.BlockRef, 12)
		for i := range refs {
			refs[i] = ref(uint64(i+1), byte(i+1))
		}
		store := storedBlocks(t, refs...)

		// The history keeps the newest maxUndoBlocks frames, so the head plus
		// their previous blocks are reachable.
		want := make([]domain.BlockRef, 0, maxUndoBlocks+1)
		for number := uint64(12); number > 12-(maxUndoBlocks+1); number-- {
			want = append(want, ref(number, byte(number)))
		}
		if got := store.UndoRefs(); !slices.Equal(got, want) {
			t.Fatalf("UndoRefs() = %+v, want %+v", got, want)
		}
	})

	t.Run("zero hashes and duplicates are skipped", func(t *testing.T) {
		store := New()
		if err := store.BeginBlock(domain.BlockRef{Number: 1}); err != nil {
			t.Fatalf("BeginBlock(zero hash): %v", err)
		}
		store.Commit()
		head := ref(2, 0x22)
		if err := store.BeginBlock(head); err != nil {
			t.Fatalf("BeginBlock(head): %v", err)
		}
		store.Commit()
		// A repeated ref at the same number is legal and must not duplicate.
		if err := store.BeginBlock(head); err != nil {
			t.Fatalf("BeginBlock(head again): %v", err)
		}
		store.Commit()

		want := []domain.BlockRef{head}
		if got := store.UndoRefs(); !slices.Equal(got, want) {
			t.Fatalf("UndoRefs() = %+v, want %+v", got, want)
		}
	})

	t.Run("empty store", func(t *testing.T) {
		if got := New().UndoRefs(); len(got) != 0 {
			t.Fatalf("UndoRefs() = %+v, want empty", got)
		}
	})
}
