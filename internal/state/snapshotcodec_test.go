package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"math/big"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// snapshotCodecStore returns a store holding a V3 pool with tick state, a V2
// pool with reserves, a V2 pool with nil reserves, and one dirty marker.
func snapshotCodecStore(t *testing.T) *Store {
	t.Helper()
	store := New()
	head := domain.BlockRef{Number: 4_200, Hash: domain.Hash{0x0b, 0x0c}, ParentHash: domain.Hash{0x0b, 0x0d}, Timestamp: 1_700_000_123, L1Block: 77}
	if err := store.BeginBlock(head); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}

	v3 := testAddress(0x60)
	if err := store.Upsert(v3State(v3)); err != nil {
		t.Fatalf("Upsert v3: %v", err)
	}
	if err := store.ApplyV3(v3Key(v3), initializeEvent(), head.Number); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	if err := store.ApplyV3(v3Key(v3), mintEvent(-120, 120, 1_000_000), head.Number); err != nil {
		t.Fatalf("ApplyV3 mint: %v", err)
	}

	v2 := testAddress(0x61)
	if err := store.Upsert(v2State(v2)); err != nil {
		t.Fatalf("Upsert v2: %v", err)
	}
	sparse := v2State(testAddress(0x62))
	sparse.Reserve0 = nil
	sparse.Reserve1 = nil
	if err := store.Upsert(sparse); err != nil {
		t.Fatalf("Upsert sparse v2: %v", err)
	}

	store.ClearDirty()
	if err := store.Upsert(v2State(v2)); err != nil {
		t.Fatalf("Upsert dirty v2: %v", err)
	}
	return store
}

// loadableSnapshotWire encodes the fixture store and decodes it back into the
// wire form so a test can break exactly one field.
func loadableSnapshotWire(t *testing.T) snapshotWire {
	t.Helper()
	data, err := snapshotCodecStore(t).EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	var wire snapshotWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return wire
}

func assertSliceEqual[T comparable](t *testing.T, name string, got, want []T) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Fatalf("%s = %v, want %v", name, got, want)
	}
}

func TestSnapshotCodecRoundTrip(t *testing.T) {
	source := snapshotCodecStore(t)
	v3 := testAddress(0x60)
	v2 := testAddress(0x61)
	sparse := testAddress(0x62)

	wantBlock := source.Block()
	wantV3, ok := source.Pool(v3Key(v3))
	if !ok {
		t.Fatalf("Pool(%s) missing in fixture", v3)
	}
	wantV2, ok := source.Pool(v2Key(v2))
	if !ok {
		t.Fatalf("Pool(%s) missing in fixture", v2)
	}
	wantSparse, ok := source.Pool(v2Key(sparse))
	if !ok {
		t.Fatalf("Pool(%s) missing in fixture", sparse)
	}
	wantDeep, ok := source.Deep(v3Key(v3))
	if !ok {
		t.Fatalf("Deep(%s) missing in fixture", v3)
	}
	wantDirty := source.Dirty()

	data, err := source.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}

	restored := New()
	if err := restored.LoadSnapshot(data); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}

	if got := restored.Block(); got != wantBlock {
		t.Fatalf("Block() = %+v, want %+v", got, wantBlock)
	}
	for _, tc := range []struct {
		key  domain.PoolKey
		want pool.State
	}{
		{key: v3Key(v3), want: wantV3},
		{key: v2Key(v2), want: wantV2},
		{key: v2Key(sparse), want: wantSparse},
	} {
		got, ok := restored.Pool(tc.key)
		if !ok {
			t.Fatalf("Pool(%s) missing after load", formatPoolKey(tc.key))
		}
		assertPoolEqual(t, got, tc.want)
	}
	gotDeep, ok := restored.Deep(v3Key(v3))
	if !ok {
		t.Fatalf("Deep(%s) missing after load", v3)
	}
	assertDeepEqual(t, gotDeep, wantDeep)
	if _, ok := restored.Deep(v2Key(v2)); ok {
		t.Fatalf("Deep(%s) appeared after load", v2)
	}
	assertPoolKeys(t, restored.Dirty(), wantDirty)

	if err := restored.Rollback(); !errors.Is(err, ErrNoUndoFrame) {
		t.Fatalf("Rollback after LoadSnapshot = %v, want %v", err, ErrNoUndoFrame)
	}

	again, err := restored.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot after load: %v", err)
	}
	if !bytes.Equal(data, again) {
		t.Fatalf("re-encoded snapshot differs from the loaded bytes")
	}
}

func TestEncodeSnapshotIsDeterministic(t *testing.T) {
	store := snapshotCodecStore(t)
	first, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("first EncodeSnapshot: %v", err)
	}
	second, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("second EncodeSnapshot: %v", err)
	}
	if !bytes.Equal(first, second) {
		t.Fatalf("two encodes of the same store differ")
	}
}

func TestEncodeSnapshotSortsEntries(t *testing.T) {
	store := New()
	for _, b := range []byte{0x73, 0x71, 0x72} {
		if err := store.Upsert(v3State(testAddress(b))); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	v3 := testAddress(0x72)
	if err := store.ApplyV3(v3Key(v3), initializeEvent(), 0); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	if err := store.ApplyV3(v3Key(v3), mintEvent(-120, 120, 1_000_000), 0); err != nil {
		t.Fatalf("ApplyV3 mint low: %v", err)
	}
	if err := store.ApplyV3(v3Key(v3), mintEvent(3000, 3120, 500_000), 0); err != nil {
		t.Fatalf("ApplyV3 mint high: %v", err)
	}

	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	var wire snapshotWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}

	assertSliceEqual(t, "pool addresses", wireAddresses(wire.Pools), []string{
		testAddress(0x71).Hex(), v3.Hex(), testAddress(0x73).Hex(),
	})
	assertSliceEqual(t, "deep addresses", wireDeepAddresses(wire.Deep), []string{v3.Hex()})
	assertSliceEqual(t, "dirty keys", wire.Dirty, []string{
		testAddress(0x71).Hex(), v3.Hex(), testAddress(0x73).Hex(),
	})

	if len(wire.Deep) != 1 {
		t.Fatalf("deep entries = %d, want 1", len(wire.Deep))
	}
	words := make([]int16, 0, len(wire.Deep[0].Bitmap))
	for _, word := range wire.Deep[0].Bitmap {
		words = append(words, word.Word)
	}
	assertSliceEqual(t, "bitmap words", words, []int16{-1, 0, 1})

	ticks := make([]int32, 0, len(wire.Deep[0].Ticks))
	for _, tick := range wire.Deep[0].Ticks {
		ticks = append(ticks, tick.Tick)
	}
	assertSliceEqual(t, "ticks", ticks, []int32{-120, 120, 3000, 3120})
}

func wireAddresses(pools []poolWire) []string {
	addrs := make([]string, 0, len(pools))
	for _, p := range pools {
		addrs = append(addrs, p.Address)
	}
	return addrs
}

func wireDeepAddresses(deep []deepWire) []string {
	addrs := make([]string, 0, len(deep))
	for _, d := range deep {
		addrs = append(addrs, d.Address)
	}
	return addrs
}

func TestEncodeSnapshotWireFormat(t *testing.T) {
	data, err := New().EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	want := `{"version":1,"block":{"number":0,"hash":"` + domain.Hash{}.Hex() +
		`","parent_hash":"` + domain.Hash{}.Hex() +
		`","timestamp":0,"l1_block":0},"pools":[],"deep":[],"dirty":[]}`
	if string(data) != want {
		t.Fatalf("EncodeSnapshot() = %s, want %s", data, want)
	}
}

func TestSnapshotCodecPreservesLoadedBlock(t *testing.T) {
	store := New()
	head := domain.BlockRef{Number: 4_200, Hash: domain.Hash{0x0d}, ParentHash: domain.Hash{0x0e}, Timestamp: 1_700_000_123, L1Block: 77}
	if err := store.BeginBlock(head); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}
	addr := testAddress(0x63)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	loaded := domain.BlockRef{Number: 4_150, Hash: domain.Hash{0x5c}, ParentHash: domain.Hash{0x5d}, Timestamp: 1_699_999_000, L1Block: 70}
	deep := testDeep()
	deep.LoadedBlock = loaded
	if err := store.SetDeep(v3Key(addr), deep); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}

	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	var wire snapshotWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	if len(wire.Deep) != 1 {
		t.Fatalf("deep entries = %d, want 1", len(wire.Deep))
	}
	if wire.Deep[0].LoadedBlock != blockToWire(loaded) {
		t.Fatalf("loaded_block = %+v, want %+v", wire.Deep[0].LoadedBlock, blockToWire(loaded))
	}
	if wire.Deep[0].WindowMin != deep.WindowMin || wire.Deep[0].WindowMax != deep.WindowMax {
		t.Fatalf("window wire = [%d, %d], want [%d, %d]",
			wire.Deep[0].WindowMin, wire.Deep[0].WindowMax, deep.WindowMin, deep.WindowMax)
	}

	restored := New()
	if err := restored.LoadSnapshot(data); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	got, ok := restored.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after load", addr)
	}
	assertDeepEqual(t, got, deep)
}

// TestSnapshotCodecUnknownWindowStaysZero checks the compatibility path: a deep
// state written before the window existed (or one whose window is unknown) has
// no window fields, and loading it yields zero/zero rather than an error.
func TestSnapshotCodecUnknownWindowStaysZero(t *testing.T) {
	store := New()
	addr := testAddress(0x64)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.SetDeep(v3Key(addr), pool.DeepState{
		Bitmap: map[int16]*big.Int{0: big.NewInt(1)},
		Ticks:  map[int32]pool.TickInfo{},
	}); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}

	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if bytes.Contains(data, []byte("window_min")) || bytes.Contains(data, []byte("window_max")) {
		t.Fatalf("unknown window encoded: %s", data)
	}

	restored := New()
	if err := restored.LoadSnapshot(data); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	got, ok := restored.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after load", addr)
	}
	if got.WindowMin != 0 || got.WindowMax != 0 {
		t.Fatalf("window = [%d, %d], want zero", got.WindowMin, got.WindowMax)
	}
}

// TestEncodeSnapshotPreservesUndoHistory checks that encoding for persistence
// does not clear the rollback window: two committed frames stay reachable
// after EncodeSnapshot.
func TestEncodeSnapshotPreservesUndoHistory(t *testing.T) {
	store := New()
	addr := testAddress(0x59)
	for number := uint64(10); number <= 12; number++ {
		beginBlock(t, store, number)
		upsertTick(t, store, addr, int32(number), number)
		store.Commit()
	}

	if _, err := store.EncodeSnapshot(); err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	if err := store.RollbackTo(10); err != nil {
		t.Fatalf("RollbackTo(10) after EncodeSnapshot: %v", err)
	}
	if got := store.Block().Number; got != 10 {
		t.Fatalf("head = %d, want 10", got)
	}
	pool, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("Pool(%s) missing after rollback", addr)
	}
	if pool.Tick != 10 || pool.UpdatedBlock != 10 {
		t.Fatalf("pool = tick %d block %d, want tick 10 block 10", pool.Tick, pool.UpdatedBlock)
	}
}
