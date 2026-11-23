package state

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestUpsertMarksV3ShapedPoolsStale(t *testing.T) {
	key, _ := v4SiblingKeys()
	tests := []struct {
		name  string
		state pool.State
		want  bool
	}{
		{"v3", v3State(testAddress(0x41)), true},
		{"camelot v3", withPoolKind(v3State(testAddress(0x42)), domain.PoolKindCamelotV3), true},
		{"slipstream", withPoolKind(v3State(testAddress(0x43)), domain.PoolKindSlipstream), true},
		{"v4", v4State(key), true},
		{"v2", v2State(testAddress(0x44)), false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			if err := store.Upsert(test.state); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			stale := store.DeepStale()
			if got := len(stale) == 1 && stale[0] == test.state.Key; got != test.want {
				t.Fatalf("DeepStale() = %v, want marked: %t", stale, test.want)
			}
		})
	}
}

// TestApplyV3StaleKinds checks which events invalidate tick contents. A swap
// only moves price and active liquidity, so it leaves the bitmap valid, and a
// zero-amount mint or burn changes no tick at all.
func TestApplyV3StaleKinds(t *testing.T) {
	tests := []struct {
		name string
		ev   uniswapv3.Event
		want bool
	}{
		{"initialize", initializeEvent(), true},
		{"mint", mintEvent(-60, 60, 100), true},
		{"zero mint", mintEvent(-60, 60, 0), false},
		{"burn", burnEvent(-120, 120, 100), true},
		{"zero burn", burnEvent(-120, 120, 0), false},
		{"swap", swapEvent(testQ96(), 0, 500_000), false},
		{"collect", uniswapv3.Event{Kind: uniswapv3.EventCollect}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr := testAddress(0x45)
			store := seededStore(t, addr)
			if err := store.ApplyV3(v3Key(addr), test.ev, 11); err != nil {
				t.Fatalf("ApplyV3: %v", err)
			}
			stale := store.DeepStale()
			if got := len(stale) == 1 && stale[0] == v3Key(addr); got != test.want {
				t.Fatalf("DeepStale() = %v, want marked: %t", stale, test.want)
			}
		})
	}
}

func TestSetDeepClearsStaleMarker(t *testing.T) {
	addr := testAddress(0x46)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(store.DeepStale()) != 1 {
		t.Fatalf("DeepStale() = %v, want the new pool marked", store.DeepStale())
	}
	if err := store.SetDeep(v3Key(addr), testDeep()); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}
	if stale := store.DeepStale(); len(stale) != 0 {
		t.Fatalf("DeepStale() = %v, want empty after SetDeep", stale)
	}
}

func TestRollbackRestoresStaleMarkers(t *testing.T) {
	addr := testAddress(0x47)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	beginBlock(t, store, 10)
	if err := store.ApplyV3(v3Key(addr), mintEvent(-60, 60, 100), 10); err != nil {
		t.Fatalf("ApplyV3: %v", err)
	}
	if len(store.DeepStale()) == 0 {
		t.Fatal("DeepStale() empty before rollback")
	}
	if err := store.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	assertPoolKeys(t, store.DeepStale(), []domain.PoolKey{v3Key(addr)})
}

func TestRollbackToRestoresStaleMarkers(t *testing.T) {
	addr := testAddress(0x48)
	store := New()
	beginBlock(t, store, 10)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.ApplyV3(v3Key(addr), mintEvent(-60, 60, 100), 10); err != nil {
		t.Fatalf("ApplyV3 mint: %v", err)
	}
	store.Commit()
	beginBlock(t, store, 11)
	if err := store.ApplyV3(v3Key(addr), swapEvent(testQ96(), 0, 500_000), 11); err != nil {
		t.Fatalf("ApplyV3 swap: %v", err)
	}
	store.Commit()
	if len(store.DeepStale()) == 0 {
		t.Fatal("DeepStale() empty before rollback")
	}

	if err := store.RollbackTo(10); err != nil {
		t.Fatalf("RollbackTo(10): %v", err)
	}
	assertPoolKeys(t, store.DeepStale(), []domain.PoolKey{v3Key(addr)})
}

func TestTransactionRollbackKeepsEarlierTickInvalidation(t *testing.T) {
	addr := testAddress(0x50)
	store := seededStore(t, addr)
	if err := store.ApplyV3(v3Key(addr), mintEvent(-60, 60, 100), 10); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	beginBlock(t, store, 10)
	// A failed later transaction must not make the earlier mint's ticks fresh.
	if err := store.SetDeep(v3Key(addr), pool.DeepState{}); err != nil {
		t.Fatal(err)
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}
	assertPoolKeys(t, store.DeepStale(), []domain.PoolKey{v3Key(addr)})
}

func TestRestoreReplacesStaleMarkers(t *testing.T) {
	addr := testAddress(0x4a)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if len(store.DeepStale()) == 0 {
		t.Fatal("DeepStale() empty before restore")
	}
	if err := store.Restore(New().Checkpoint()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if stale := store.DeepStale(); len(stale) != 0 {
		t.Fatalf("DeepStale() = %v, want empty after Restore", stale)
	}
}

func TestDeepStaleIsSorted(t *testing.T) {
	store := New()
	for _, b := range []byte{0x4d, 0x4b, 0x4c} {
		if err := store.Upsert(v3State(testAddress(b))); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	want := []domain.PoolKey{v3Key(testAddress(0x4b)), v3Key(testAddress(0x4c)), v3Key(testAddress(0x4d))}
	assertPoolKeys(t, store.DeepStale(), want)
}

func withPoolKind(state pool.State, kind domain.PoolKind) pool.State {
	state.Key.Kind = kind
	return state
}
