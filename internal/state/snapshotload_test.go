package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestLoadSnapshotReplacesStore(t *testing.T) {
	source := snapshotCodecStore(t)
	data, err := source.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}

	target := New()
	if err := target.BeginBlock(domain.BlockRef{Number: 1, Hash: domain.Hash{0xee}}); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}
	stale := testAddress(0x90)
	if err := target.Upsert(v3State(stale)); err != nil {
		t.Fatalf("Upsert stale: %v", err)
	}

	if err := target.LoadSnapshot(data); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if _, ok := target.Pool(v3Key(stale)); ok {
		t.Fatalf("LoadSnapshot kept pool %s that is absent from the snapshot", stale)
	}
	if got := target.Block(); got != source.Block() {
		t.Fatalf("Block() = %+v, want %+v", got, source.Block())
	}
	got, err := target.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot after load: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("loaded store encodes to different bytes than the source")
	}
}

func TestLoadSnapshotRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*snapshotWire)
	}{
		{
			name:   "version mismatch",
			mutate: func(w *snapshotWire) { w.Version = MetricStateVersion + 1 },
		},
		{
			name:   "malformed decimal",
			mutate: func(w *snapshotWire) { w.Pools[0].Liquidity = "1e6" },
		},
		{
			name:   "malformed block hash",
			mutate: func(w *snapshotWire) { w.Block.Hash = "0x1234" },
		},
		{
			name:   "malformed parent hash",
			mutate: func(w *snapshotWire) { w.Block.ParentHash = "0x1234" },
		},
		{
			name:   "short pool address",
			mutate: func(w *snapshotWire) { w.Pools[0].Address = "0x1234" },
		},
		{
			name:   "malformed token address",
			mutate: func(w *snapshotWire) { w.Pools[0].Token0 = "0xzz" },
		},
		{
			name:   "malformed bitmap word",
			mutate: func(w *snapshotWire) { w.Deep[0].Bitmap[0].Bits = strings.Repeat("g", 64) },
		},
		{
			name:   "short bitmap word",
			mutate: func(w *snapshotWire) { w.Deep[0].Bitmap[0].Bits = "ff" },
		},
		{
			name:   "duplicate pool",
			mutate: func(w *snapshotWire) { w.Pools = append(w.Pools, w.Pools[0]) },
		},
		{
			name:   "duplicate deep",
			mutate: func(w *snapshotWire) { w.Deep = append(w.Deep, w.Deep[0]) },
		},
		{
			name:   "duplicate dirty",
			mutate: func(w *snapshotWire) { w.Dirty = append(w.Dirty, w.Dirty[0]) },
		},
		{
			name:   "duplicate bitmap word",
			mutate: func(w *snapshotWire) { w.Deep[0].Bitmap = append(w.Deep[0].Bitmap, w.Deep[0].Bitmap[0]) },
		},
		{
			name:   "duplicate tick",
			mutate: func(w *snapshotWire) { w.Deep[0].Ticks = append(w.Deep[0].Ticks, w.Deep[0].Ticks[0]) },
		},
		{
			name:   "deep for unknown pool",
			mutate: func(w *snapshotWire) { w.Deep[0].Address = testAddress(0x99).Hex() },
		},
		{
			name:   "dirty for unknown pool",
			mutate: func(w *snapshotWire) { w.Dirty[0] = testAddress(0x99).Hex() },
		},
		{
			name:   "zero pool address",
			mutate: func(w *snapshotWire) { w.Pools[0].Address = domain.Address{}.Hex() },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			target := New()
			head := domain.BlockRef{Number: 9, Hash: domain.Hash{0x09}, Timestamp: 90, L1Block: 4}
			if err := target.BeginBlock(head); err != nil {
				t.Fatalf("BeginBlock: %v", err)
			}
			existing := testAddress(0x55)
			if err := target.Upsert(v3State(existing)); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			if err := target.ApplyV3(v3Key(existing), initializeEvent(), head.Number); err != nil {
				t.Fatalf("ApplyV3: %v", err)
			}
			before := target.Snapshot()

			wire := loadableSnapshotWire(t)
			test.mutate(&wire)
			data, err := json.Marshal(wire)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}

			err = target.LoadSnapshot(data)
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("LoadSnapshot error = %v, want %v", err, ErrInvalidSnapshot)
			}
			if !reflect.DeepEqual(target.Snapshot(), before) {
				t.Fatalf("failed LoadSnapshot changed the store")
			}
		})
	}
}

func TestLoadSnapshotRejectsGarbage(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "truncated json", data: []byte("{")},
		{name: "json null", data: []byte("null")},
		{name: "missing block hash", data: []byte(`{"version":1}`)},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := New()
			if err := store.Upsert(v3State(testAddress(0x56))); err != nil {
				t.Fatalf("Upsert: %v", err)
			}
			before := store.Snapshot()

			err := store.LoadSnapshot(test.data)
			if !errors.Is(err, ErrInvalidSnapshot) {
				t.Fatalf("LoadSnapshot error = %v, want %v", err, ErrInvalidSnapshot)
			}
			if !reflect.DeepEqual(store.Snapshot(), before) {
				t.Fatalf("failed LoadSnapshot changed the store")
			}
		})
	}
}

func TestLoadSnapshotInvalidKeepsUndoFrame(t *testing.T) {
	store := New()
	prev := domain.BlockRef{Number: 3, Hash: domain.Hash{0x03}}
	head := domain.BlockRef{Number: 4, Hash: domain.Hash{0x04}}
	if err := store.BeginBlock(prev); err != nil {
		t.Fatalf("BeginBlock(prev): %v", err)
	}
	store.Commit()
	if err := store.BeginBlock(head); err != nil {
		t.Fatalf("BeginBlock(head): %v", err)
	}
	addr := testAddress(0x57)
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}

	if err := store.LoadSnapshot([]byte(`{"version":42}`)); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("LoadSnapshot error = %v, want %v", err, ErrInvalidSnapshot)
	}
	if err := store.Rollback(); err != nil {
		t.Fatalf("invalid LoadSnapshot dropped the undo frame: %v", err)
	}
	if got := store.Block(); got != prev {
		t.Fatalf("Block() after rollback = %+v, want %+v", got, prev)
	}
	if _, ok := store.Pool(v3Key(addr)); ok {
		t.Fatalf("Rollback kept pool %s created in the frame", addr)
	}
}
