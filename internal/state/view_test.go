package state

import (
	"errors"
	"math/big"
	"sync"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestViewSurvivesConcurrentWritesAndReadMutations(t *testing.T) {
	addr := testAddress(0x61)
	store := seededStore(t, addr)
	key := v3Key(addr)
	if err := store.SetDeep(key, pool.DeepState{Bitmap: map[int16]*big.Int{0: big.NewInt(123)}}); err != nil {
		t.Fatal(err)
	}
	view := store.View()
	wantBlock := store.Block()
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for range 100 {
			shallow, ok := view.Pool(key)
			deep, loaded := view.Deep(key)
			if !ok || !loaded || deep.Bitmap[0].Int64() != 123 {
				t.Error("immutable view changed during store writes")
				return
			}
			deep.Bitmap[0].SetInt64(0)
			shallow.SqrtPriceX96.SetInt64(0)
		}
	}()
	for range 100 {
		if err := store.Upsert(v3State(addr)); err != nil {
			t.Fatal(err)
		}
		if err := store.SetDeep(key, pool.DeepState{Bitmap: map[int16]*big.Int{0: big.NewInt(456)}}); err != nil {
			t.Fatal(err)
		}
	}
	readers.Wait()
	if view.Block() != wantBlock {
		t.Fatalf("unexpected view block: %+v", view.Block())
	}
}

func TestViewDoesNotExposeStaleTicks(t *testing.T) {
	addr := testAddress(0x62)
	store := seededStore(t, addr)
	before := store.View()
	if err := store.ApplyV3(v3Key(addr), mintEvent(-60, 60, 100), 10); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.View().Deep(v3Key(addr)); ok {
		t.Fatal("stale tick state exposed to quote workers")
	}
	if _, ok := before.Deep(v3Key(addr)); !ok {
		t.Fatal("new invalidation changed an earlier immutable view")
	}
}

func TestForkKeepsIndependentWritesAndStaleMarkers(t *testing.T) {
	addr := testAddress(0x65)
	store := seededStore(t, addr)
	view := store.View()
	fork := view.Fork()
	if err := fork.ApplyV3(v3Key(addr), mintEvent(-60, 60, 100), 10); err != nil {
		t.Fatal(err)
	}
	if _, ok := fork.View().Deep(v3Key(addr)); ok {
		t.Fatal("fork exposed its invalidated ticks")
	}
	if _, ok := view.Deep(v3Key(addr)); !ok || len(store.DeepStale()) != 0 {
		t.Fatal("fork mutation invalidated its source snapshot")
	}
	stale := fork.View()
	other := stale.Fork()
	if err := other.SetDeep(v3Key(addr), pool.DeepState{Bitmap: map[int16]*big.Int{0: big.NewInt(123)}}); err != nil {
		t.Fatal(err)
	}
	if _, ok := stale.Deep(v3Key(addr)); ok || len(fork.DeepStale()) == 0 {
		t.Fatal("dependency refresh changed another writer's stale markers")
	}
}

func TestImmutableViewSnapshotUsesVerifiedFinalMetadata(t *testing.T) {
	addr := testAddress(0x63)
	store := seededStore(t, addr)
	view := store.View()
	ref := store.Block()
	ref.Timestamp, ref.L1Block = 123, 456
	data, err := view.EncodeSnapshotAt(ref)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(v3State(testAddress(0x64))); err != nil {
		t.Fatal(err)
	}
	restored := New()
	if err := restored.LoadSnapshot(data); err != nil {
		t.Fatal(err)
	}
	if restored.Block() != ref || len(restored.Pools()) != 1 || len(restored.Dirty()) != 0 {
		t.Fatal("background snapshot mixed mutable state or lost final block metadata")
	}
	ref.Hash = domain.Hash{0xff}
	if _, err := view.EncodeSnapshotAt(ref); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatal("unrelated header accepted for snapshot")
	}
}
