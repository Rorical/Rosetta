package state

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// v4State builds a shallow V4 pool under key. V4 pools share the PoolManager
// address, so the key alone tells two of them apart.
func v4State(key domain.PoolKey) pool.State {
	return pool.State{
		Key:          key,
		Token0:       domain.Address{0xaa},
		Token1:       domain.Address{0xbb},
		Fee:          500,
		ProtocolFee:  0x07d07d,
		TickSpacing:  10,
		SqrtPriceX96: testQ96(),
		Liquidity:    big.NewInt(1_000_000),
	}
}

// v4SiblingKeys returns two V4 keys behind one PoolManager address.
func v4SiblingKeys() (domain.PoolKey, domain.PoolKey) {
	manager := testAddress(0x90)
	first := domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{0x01}}
	second := domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{0x02}, Hooks: domain.Address{0xcc}}
	return first, second
}

func TestV4SiblingsKeepSeparateShallowAndDeepState(t *testing.T) {
	first, second := v4SiblingKeys()
	store := New()

	if err := store.Upsert(v4State(first)); err != nil {
		t.Fatalf("Upsert first: %v", err)
	}
	if err := store.Upsert(v4State(second)); err != nil {
		t.Fatalf("Upsert second: %v", err)
	}
	firstDeep := testDeep()
	firstDeep.LoadedBlock = domain.BlockRef{Number: 10, Hash: domain.Hash{0x0a}}
	secondDeep := testDeep()
	secondDeep.LoadedBlock = domain.BlockRef{Number: 11, Hash: domain.Hash{0x0b}}
	if err := store.SetDeep(first, firstDeep); err != nil {
		t.Fatalf("SetDeep first: %v", err)
	}
	if err := store.SetDeep(second, secondDeep); err != nil {
		t.Fatalf("SetDeep second: %v", err)
	}

	if got := len(store.Pools()); got != 2 {
		t.Fatalf("Pools() = %d entries, want 2", got)
	}
	storedFirst, ok := store.Pool(first)
	if !ok {
		t.Fatal("first V4 pool missing")
	}
	storedSecond, ok := store.Pool(second)
	if !ok {
		t.Fatal("second V4 pool missing")
	}
	if storedFirst.Key != first || storedSecond.Key != second {
		t.Fatalf("pools = %+v, %+v; want keys %+v, %+v",
			storedFirst.Key, storedSecond.Key, first, second)
	}
	if got, _ := store.Deep(first); got.LoadedBlock != firstDeep.LoadedBlock {
		t.Fatalf("first deep loaded block = %+v, want %+v", got.LoadedBlock, firstDeep.LoadedBlock)
	}
	if got, _ := store.Deep(second); got.LoadedBlock != secondDeep.LoadedBlock {
		t.Fatalf("second deep loaded block = %+v, want %+v", got.LoadedBlock, secondDeep.LoadedBlock)
	}

	// First key sorts before second: same address, then kind, then pool ID.
	assertPoolKeys(t, store.Dirty(), []domain.PoolKey{first, second})
}

func TestV4SiblingsSnapshotRoundTrip(t *testing.T) {
	first, second := v4SiblingKeys()
	store := New()
	for _, key := range []domain.PoolKey{first, second} {
		if err := store.Upsert(v4State(key)); err != nil {
			t.Fatalf("Upsert %s: %v", formatPoolKey(key), err)
		}
		if err := store.SetDeep(key, testDeep()); err != nil {
			t.Fatalf("SetDeep %s: %v", formatPoolKey(key), err)
		}
	}

	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	var wire snapshotWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(wire.Pools) != 2 {
		t.Fatalf("pools in wire = %d, want 2", len(wire.Pools))
	}
	if wire.Pools[0].PoolID != first.PoolID.Hex() {
		t.Fatalf("first pool_id = %q, want %q", wire.Pools[0].PoolID, first.PoolID.Hex())
	}
	if wire.Pools[1].Hooks != second.Hooks.Hex() {
		t.Fatalf("second hooks = %q, want %q", wire.Pools[1].Hooks, second.Hooks.Hex())
	}
	if got, want := wire.Pools[0].ProtocolFee, v4State(first).ProtocolFee; got != want {
		t.Fatalf("first protocol_fee = %d, want %d", got, want)
	}

	restored := New()
	if err := restored.LoadSnapshot(data); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	for _, key := range []domain.PoolKey{first, second} {
		state, ok := restored.Pool(key)
		if !ok {
			t.Fatalf("pool %s missing after load", formatPoolKey(key))
		}
		assertPoolEqual(t, state, v4State(key))
		if _, ok := restored.Deep(key); !ok {
			t.Fatalf("deep state %s missing after load", formatPoolKey(key))
		}
	}
	again, err := restored.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot after load: %v", err)
	}
	if string(again) != string(data) {
		t.Fatal("re-encoded snapshot differs from the loaded bytes")
	}
}

// TestLegacySnapshotWithoutPoolIdentity loads a document in the version 1
// shape, before pool_id, hooks, and the deep kind existed. Missing fields must
// decode to the zero values and the deep entry must resolve through its
// address.
func TestLegacySnapshotWithoutPoolIdentity(t *testing.T) {
	store := seededStore(t, testAddress(0x91))
	if err := store.SetDeep(v3Key(testAddress(0x91)), testDeep()); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}
	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	legacy := stripPoolIdentityFields(t, data)

	restored := New()
	if err := restored.LoadSnapshot(legacy); err != nil {
		t.Fatalf("LoadSnapshot(legacy): %v", err)
	}
	if _, ok := restored.Pool(v3Key(testAddress(0x91))); !ok {
		t.Fatal("legacy shallow pool missing")
	}
	if _, ok := restored.Deep(v3Key(testAddress(0x91))); !ok {
		t.Fatal("legacy deep state missing")
	}
}

// stripPoolIdentityFields removes the fields added for V4 identity from every
// pool and deep entry, which produces the version 1 document shape.
func stripPoolIdentityFields(t *testing.T, data []byte) []byte {
	t.Helper()
	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	for _, field := range []string{"pools", "deep"} {
		entries, ok := document[field]
		if !ok {
			continue
		}
		var objects []map[string]json.RawMessage
		if err := json.Unmarshal(entries, &objects); err != nil {
			t.Fatalf("json.Unmarshal %s: %v", field, err)
		}
		for _, object := range objects {
			for _, removed := range []string{"pool_id", "hooks", "kind"} {
				if field == "pools" && removed == "kind" {
					continue
				}
				delete(object, removed)
			}
		}
		encoded, err := json.Marshal(objects)
		if err != nil {
			t.Fatalf("json.Marshal %s: %v", field, err)
		}
		document[field] = encoded
	}
	stripped, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("json.Marshal document: %v", err)
	}
	return stripped
}
