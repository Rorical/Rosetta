package state

import (
	"errors"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
)

func TestApplyV3SuccessMutatesAndMarksDirty(t *testing.T) {
	addr := testAddress(0x21)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.ApplyV3(v3Key(addr), initializeEvent(), 0); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	store.ClearDirty()

	nextSqrt := new(big.Int).Lsh(big.NewInt(3), 95)
	if err := store.ApplyV3(v3Key(addr), swapEvent(nextSqrt, 42, 750_000), 77); err != nil {
		t.Fatalf("ApplyV3 swap: %v", err)
	}

	stored, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) not found", addr)
	}
	assertIntEqual(t, "sqrt price", stored.SqrtPriceX96, nextSqrt)
	assertIntEqual(t, "liquidity", stored.Liquidity, big.NewInt(750_000))
	if stored.Tick != 42 {
		t.Fatalf("tick = %d, want 42", stored.Tick)
	}
	if stored.UpdatedBlock != 77 {
		t.Fatalf("updated block = %d, want 77", stored.UpdatedBlock)
	}
	if _, ok := store.Deep(v3Key(addr)); !ok {
		t.Fatalf("Deep(%s) not stored after initialize", addr)
	}
	assertPoolKeys(t, store.Dirty(), []domain.PoolKey{v3Key(addr)})
}

func TestApplyV3FailureLeavesStoreUnchanged(t *testing.T) {
	addr := testAddress(0x22)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.ApplyV3(v3Key(addr), initializeEvent(), 0); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	store.ClearDirty()

	before, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) not found", addr)
	}
	beforeDeep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) not found", addr)
	}

	err := store.ApplyV3(v3Key(addr), burnEvent(-10, 10, -1), 0)
	if !errors.Is(err, uniswapv3.ErrInvalidEvent) {
		t.Fatalf("ApplyV3 burn error = %v, want %v", err, uniswapv3.ErrInvalidEvent)
	}

	after, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after failed event", addr)
	}
	assertPoolEqual(t, after, before)

	afterDeep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after failed event", addr)
	}
	assertDeepEqual(t, afterDeep, beforeDeep)

	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("failed event marked pools dirty: %v", dirty)
	}
}

func TestApplyV3MissingPool(t *testing.T) {
	store := New()
	addr := testAddress(0x23)
	err := store.ApplyV3(v3Key(addr), swapEvent(testQ96(), 0, 1), 0)
	if !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("ApplyV3 error = %v, want %v", err, ErrPoolNotFound)
	}
	if _, ok := store.Pool(v3Key(addr)); ok {
		t.Fatalf("ApplyV3 created missing pool %s", addr)
	}
	if _, ok := store.Deep(v3Key(addr)); ok {
		t.Fatalf("ApplyV3 created deep state for missing pool %s", addr)
	}
	if dirty := store.Dirty(); len(dirty) != 0 {
		t.Fatalf("rejected ApplyV3 marked pools dirty: %v", dirty)
	}
}

func TestApplyV3AcceptsV3ShapedKinds(t *testing.T) {
	kinds := []domain.PoolKind{
		domain.PoolKindV3,
		domain.PoolKindCamelotV3,
		domain.PoolKindSlipstream,
		domain.PoolKindV4,
	}
	for _, kind := range kinds {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			addr := testAddress(0x26)
			initial := v3State(addr)
			initial.Key.Kind = kind
			store := New()
			if err := store.Upsert(initial); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			if err := store.ApplyV3(initial.Key, initializeEvent(), 0); err != nil {
				t.Fatalf("ApplyV3 initialize: %v", err)
			}
			store.ClearDirty()

			nextSqrt := new(big.Int).Lsh(big.NewInt(3), 95)
			if err := store.ApplyV3(initial.Key, swapEvent(nextSqrt, 42, 750_000), 77); err != nil {
				t.Fatalf("ApplyV3 swap: %v", err)
			}

			stored, ok := store.Pool(initial.Key)
			if !ok {
				t.Fatalf("Pool(%s) not found", addr)
			}
			if stored.Key.Kind != kind {
				t.Fatalf("kind = %d, want %d", stored.Key.Kind, kind)
			}
			assertIntEqual(t, "sqrt price", stored.SqrtPriceX96, nextSqrt)
			assertIntEqual(t, "liquidity", stored.Liquidity, big.NewInt(750_000))
			if stored.Tick != 42 || stored.UpdatedBlock != 77 {
				t.Fatalf("tick/block = %d/%d, want 42/77", stored.Tick, stored.UpdatedBlock)
			}
			assertPoolKeys(t, store.Dirty(), []domain.PoolKey{initial.Key})
		})
	}
}

// TestApplyV3AcceptsV4Swap checks the V4 sibling key: the pool ID is part of
// the store key, and the V3 reducer advances a V4 pool's price and liquidity.
func TestApplyV3AcceptsV4Swap(t *testing.T) {
	addr := testAddress(0x27)
	key := domain.PoolKey{Address: addr, Kind: domain.PoolKindV4, PoolID: domain.Hash{0x01}}
	initial := v3State(addr)
	initial.Key = key
	initial.ProtocolFee = 0x07d07d

	store := New()
	if err := store.Upsert(initial); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.ApplyV3(key, initializeEvent(), 0); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	store.ClearDirty()

	nextSqrt := new(big.Int).Lsh(big.NewInt(3), 95)
	if err := store.ApplyV3(key, swapEvent(nextSqrt, -42, 750_000), 77); err != nil {
		t.Fatalf("ApplyV3 swap: %v", err)
	}

	stored, ok := store.Pool(key)
	if !ok {
		t.Fatalf("Pool(%s) not found", key.PoolID.Hex())
	}
	if stored.Key != key {
		t.Fatalf("key = %+v, want %+v", stored.Key, key)
	}
	assertIntEqual(t, "sqrt price", stored.SqrtPriceX96, nextSqrt)
	assertIntEqual(t, "liquidity", stored.Liquidity, big.NewInt(750_000))
	if stored.Tick != -42 || stored.UpdatedBlock != 77 {
		t.Fatalf("tick/block = %d/%d, want -42/77", stored.Tick, stored.UpdatedBlock)
	}
	assertPoolKeys(t, store.Dirty(), []domain.PoolKey{key})
}

func TestApplyV3RejectsNonV3Pool(t *testing.T) {
	tests := []struct {
		name string
		kind domain.PoolKind
	}{
		{"v2", domain.PoolKindV2},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			addr := testAddress(0x25)
			store := New()
			initial := v3State(addr)
			initial.Key.Kind = test.kind
			if err := store.Upsert(initial); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			before, ok := store.Pool(initial.Key)
			if !ok {
				t.Fatalf("Pool(%s) not found", addr)
			}
			store.ClearDirty()

			err := store.ApplyV3(initial.Key, initializeEvent(), 0)
			if !errors.Is(err, ErrInvalidPoolKind) {
				t.Fatalf("ApplyV3 on kind %d error = %v, want %v", test.kind, err, ErrInvalidPoolKind)
			}

			after, ok := store.Pool(initial.Key)
			if !ok {
				t.Fatalf("Pool(%s) missing after rejected event", addr)
			}
			assertPoolEqual(t, after, before)
			if _, ok := store.Deep(initial.Key); ok {
				t.Fatalf("rejected V3 event created deep state for kind %d", test.kind)
			}
			if dirty := store.Dirty(); len(dirty) != 0 {
				t.Fatalf("rejected V3 event marked pools dirty: %v", dirty)
			}
		})
	}
}
