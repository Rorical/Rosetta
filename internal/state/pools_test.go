package state

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestPoolsSortedByAddress(t *testing.T) {
	store := New()
	for _, addr := range []domain.Address{testAddress(0x33), testAddress(0x11), testAddress(0x22)} {
		if err := store.Upsert(v3State(addr)); err != nil {
			t.Fatalf("Upsert(%s): %v", addr, err)
		}
	}

	pools := store.Pools()
	want := []domain.PoolKey{v3Key(testAddress(0x11)), v3Key(testAddress(0x22)), v3Key(testAddress(0x33))}
	if len(pools) != len(want) {
		t.Fatalf("Pools() length = %d, want %d", len(pools), len(want))
	}
	for i := range want {
		if pools[i].Key != want[i] {
			t.Fatalf("Pools()[%d] = %s, want %s", i, formatPoolKey(pools[i].Key), formatPoolKey(want[i]))
		}
	}
}

func TestPoolsEmptyStore(t *testing.T) {
	if pools := New().Pools(); len(pools) != 0 {
		t.Fatalf("Pools() = %v, want empty", pools)
	}
}

func TestPoolsAreIsolatedCopies(t *testing.T) {
	store := New()
	addr := testAddress(0x11)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	pools := store.Pools()
	pools[0].SqrtPriceX96.SetInt64(1)
	pools[0].Liquidity.SetInt64(2)
	pools[0].Tick = 99

	stored, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing", addr)
	}
	assertPoolEqual(t, stored, v3State(addr))

	// A later call must not observe the mutations either.
	if got := store.Pools()[0]; got.Tick != 0 {
		t.Fatalf("second Pools()[0].Tick = %d, want 0", got.Tick)
	}
}
