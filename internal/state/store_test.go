package state

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestUpsertAndPoolAreIsolated(t *testing.T) {
	store := New()
	addr := testAddress(0x11)
	input := v3State(addr)
	if err := store.Upsert(input); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	stored, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) not found", addr)
	}
	assertPoolEqual(t, stored, input)

	stored.SqrtPriceX96.SetInt64(1)
	stored.Liquidity.SetInt64(2)
	stored.Tick = 99

	again, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) not found after mutating a copy", addr)
	}
	assertPoolEqual(t, again, v3State(addr))

	// Mutating the caller's value after Upsert must not change the store.
	input.Liquidity.SetInt64(42)
	third, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) not found after mutating the input", addr)
	}
	assertIntEqual(t, "liquidity", third.Liquidity, big.NewInt(1_000_000))
}

func TestZeroStoreAcceptsWrites(t *testing.T) {
	var store Store
	addr := testAddress(0x14)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert on zero Store: %v", err)
	}
	if _, ok := store.Pool(v3Key(addr)); !ok {
		t.Fatalf("Pool(%s) missing after Upsert on zero Store", addr)
	}
	if err := store.ApplyV3(v3Key(addr), initializeEvent(), 0); err != nil {
		t.Fatalf("ApplyV3 on zero Store: %v", err)
	}
}

func TestUpsertRejectsZeroAddress(t *testing.T) {
	store := New()
	err := store.Upsert(pool.State{})
	if !errors.Is(err, pool.ErrInvalidState) {
		t.Fatalf("Upsert(zero address) error = %v, want %v", err, pool.ErrInvalidState)
	}
	if len(store.Dirty()) != 0 {
		t.Fatalf("rejected Upsert marked pools dirty: %v", store.Dirty())
	}
}

func TestUpsertKeepsDeepStateWhenIdentityUnchanged(t *testing.T) {
	addr := testAddress(0x26)
	store := seededStore(t, addr)
	beforeDeep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing before Upsert", addr)
	}

	refreshed := v3State(addr)
	refreshed.SqrtPriceX96 = new(big.Int).Lsh(big.NewInt(2), 96)
	refreshed.Liquidity = big.NewInt(500_000)
	refreshed.Tick = -7
	refreshed.UpdatedBlock = 77
	if err := store.Upsert(refreshed); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	afterDeep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Upsert dropped deep state for unchanged identity")
	}
	assertDeepEqual(t, afterDeep, beforeDeep)

	stored, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after Upsert", addr)
	}
	assertPoolEqual(t, stored, refreshed)
}

func TestUpsertDropsDeepStateWhenIdentityChanges(t *testing.T) {
	addr := testAddress(0x27)
	tests := []struct {
		name  string
		state func() pool.State
	}{
		{
			name:  "kind",
			state: func() pool.State { return v2State(addr) },
		},
		{
			name: "token0",
			state: func() pool.State {
				p := v3State(addr)
				p.Token0 = domain.Address{0xcc}
				return p
			},
		},
		{
			name: "token1",
			state: func() pool.State {
				p := v3State(addr)
				p.Token1 = domain.Address{0xdd}
				return p
			},
		},
		{
			name: "tick spacing",
			state: func() pool.State {
				p := v3State(addr)
				p.TickSpacing = 60
				return p
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := seededStore(t, addr)
			if _, ok := store.Deep(v3Key(addr)); !ok {
				t.Fatalf("Deep(%s) missing before Upsert", addr)
			}

			next := test.state()
			if err := store.Upsert(next); err != nil {
				t.Fatalf("Upsert: %v", err)
			}

			if _, ok := store.Deep(v3Key(addr)); ok {
				t.Fatalf("Upsert kept deep state after identity change")
			}
			stored, ok := store.Pool(next.Key)
			if !ok {
				t.Fatalf("Pool(%s) missing after Upsert", formatPoolKey(next.Key))
			}
			assertPoolEqual(t, stored, next)
		})
	}
}

func TestPoolAndDeepMissing(t *testing.T) {
	store := New()
	addr := testAddress(0x12)
	if _, ok := store.Pool(v3Key(addr)); ok {
		t.Fatalf("Pool(%s) reported a missing pool", addr)
	}
	if _, ok := store.Deep(v3Key(addr)); ok {
		t.Fatalf("Deep(%s) reported missing deep state", addr)
	}
}

func TestDeepIsIsolatedFromStore(t *testing.T) {
	addr := testAddress(0x13)
	store := seededStore(t, addr)

	deep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) not found", addr)
	}
	if len(deep.Ticks) != 2 || len(deep.Bitmap) != 2 {
		t.Fatalf("seeded deep state has %d ticks and %d bitmap words, want 2 and 2", len(deep.Ticks), len(deep.Bitmap))
	}

	deep.Ticks[-120].LiquidityGross.SetInt64(0)
	deep.Ticks[120].LiquidityNet.SetInt64(0)
	deep.Bitmap[0].SetInt64(0)
	delete(deep.Ticks, -120)

	again, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) not found after mutating a copy", addr)
	}
	if len(again.Ticks) != 2 || len(again.Bitmap) != 2 {
		t.Fatalf("store deep state changed: %d ticks, %d bitmap words", len(again.Ticks), len(again.Bitmap))
	}
	assertIntEqual(t, "lower gross", again.Ticks[-120].LiquidityGross, big.NewInt(1_000_000))
	assertIntEqual(t, "upper net", again.Ticks[120].LiquidityNet, big.NewInt(-1_000_000))
	if bit := again.Bitmap[-1].Bit(244); bit != 1 {
		t.Fatalf("lower tick bit cleared from store bitmap")
	}
	if bit := again.Bitmap[0].Bit(12); bit != 1 {
		t.Fatalf("upper tick bit cleared from store bitmap")
	}
}

func TestDirtySortedAndCleared(t *testing.T) {
	store := New()
	for _, addr := range []domain.Address{testAddress(0x30), testAddress(0x05), testAddress(0x20)} {
		if err := store.Upsert(v3State(addr)); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	want := []domain.PoolKey{v3Key(testAddress(0x05)), v3Key(testAddress(0x20)), v3Key(testAddress(0x30))}
	assertPoolKeys(t, store.Dirty(), want)

	if err := store.Upsert(v3State(testAddress(0x20))); err != nil {
		t.Fatalf("Upsert existing: %v", err)
	}
	assertPoolKeys(t, store.Dirty(), want)

	got := store.Dirty()
	got[0] = v3Key(testAddress(0xff))
	assertPoolKeys(t, store.Dirty(), want)

	store.ClearDirty()
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("Dirty() after ClearDirty = %v, want empty", dirty)
	}
}

func TestBeginBlock(t *testing.T) {
	first := domain.BlockRef{Number: 10, Hash: domain.Hash{0x01}, Timestamp: 100, L1Block: 5}
	second := domain.BlockRef{Number: 11, Hash: domain.Hash{0x02}, Timestamp: 200, L1Block: 6}
	sameNumberNewHash := domain.BlockRef{Number: 11, Hash: domain.Hash{0x03}, Timestamp: 201, L1Block: 7}

	tests := []struct {
		name    string
		refs    []domain.BlockRef
		wantErr bool
		want    domain.BlockRef
	}{
		{
			name: "increasing accepted",
			refs: []domain.BlockRef{first, second},
			want: second,
		},
		{
			name: "same number accepted",
			refs: []domain.BlockRef{first, second, sameNumberNewHash},
			want: sameNumberNewHash,
		},
		{
			name:    "decreasing rejected",
			refs:    []domain.BlockRef{first, second, {Number: 10}},
			wantErr: true,
			want:    second,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			for i, ref := range test.refs {
				err := store.BeginBlock(ref)
				last := i == len(test.refs)-1
				if last && test.wantErr {
					if !errors.Is(err, ErrBlockOutOfOrder) {
						t.Fatalf("BeginBlock(%d) error = %v, want %v", ref.Number, err, ErrBlockOutOfOrder)
					}
					continue
				}
				if err != nil {
					t.Fatalf("BeginBlock(%d): %v", ref.Number, err)
				}
			}
			if got := store.Block(); got != test.want {
				t.Fatalf("Block() = %+v, want %+v", got, test.want)
			}
		})
	}
}

// TestUpsertKindChangeDropsStaleDirty checks that replacing a pool with another
// kind at the same address leaves no dirty marker behind for the removed key.
func TestUpsertKindChangeDropsStaleDirty(t *testing.T) {
	addr := testAddress(0x29)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert v3: %v", err)
	}
	if err := store.Upsert(v2State(addr)); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}

	want := []domain.PoolKey{v2Key(addr)}
	assertPoolKeys(t, store.Dirty(), want)
	if _, ok := store.Pool(v3Key(addr)); ok {
		t.Fatal("V3 pool survived a kind change at its address")
	}
}

// TestUpsertKindChangeRollsBack checks that replacing a pool with another kind
// at the same address removes the old entry and its deep state, and that a
// rollback restores both.
func TestUpsertKindChangeRollsBack(t *testing.T) {
	addr := testAddress(0x28)
	store := seededStore(t, addr)
	if err := store.BeginBlock(domain.BlockRef{Number: 11, Hash: domain.Hash{0x0b}}); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}

	if err := store.Upsert(v2State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if _, ok := store.Pool(v3Key(addr)); ok {
		t.Fatal("V3 pool survived a kind change at its address")
	}
	if _, ok := store.Pool(v2Key(addr)); !ok {
		t.Fatal("V2 pool missing after a kind change")
	}

	if err := store.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	if _, ok := store.Pool(v2Key(addr)); ok {
		t.Fatal("rollback kept the V2 pool")
	}
	if _, ok := store.Pool(v3Key(addr)); !ok {
		t.Fatal("rollback did not restore the V3 pool")
	}
	if _, ok := store.Deep(v3Key(addr)); !ok {
		t.Fatal("rollback did not restore the V3 deep state")
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("Dirty() after rollback = %v, want empty", dirty)
	}
}
