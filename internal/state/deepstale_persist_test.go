package state

import (
	"encoding/json"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// TestSnapshotRoundTripsStaleMarkers checks that a checkpoint, the encoded
// document, and a restore all carry the markers that force a deep reload.
func TestSnapshotRoundTripsStaleMarkers(t *testing.T) {
	addr := testAddress(0x51)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	want := []domain.PoolKey{v3Key(addr)}
	assertPoolKeys(t, store.DeepStale(), want)

	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}
	var wire snapshotWire
	if err := json.Unmarshal(data, &wire); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	assertSliceEqual(t, "stale keys", wire.DeepStale, []string{addr.Hex()})

	restored := New()
	if err := restored.LoadSnapshot(data); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	assertPoolKeys(t, restored.DeepStale(), want)

	// A checkpoint carries the markers directly, without a JSON round trip.
	checkpointed := New()
	if err := checkpointed.Restore(store.Checkpoint()); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertPoolKeys(t, checkpointed.DeepStale(), want)
}

// TestSnapshotWithoutStaleFieldLoadsEmpty checks compatibility with documents
// written before the field existed.
func TestSnapshotWithoutStaleFieldLoadsEmpty(t *testing.T) {
	addr := testAddress(0x52)
	store := New()
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	data, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatalf("EncodeSnapshot: %v", err)
	}

	var document map[string]json.RawMessage
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	delete(document, "deep_stale")
	withoutField, err := json.Marshal(document)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}

	restored := New()
	if err := restored.LoadSnapshot(withoutField); err != nil {
		t.Fatalf("LoadSnapshot: %v", err)
	}
	if stale := restored.DeepStale(); len(stale) != 0 {
		t.Fatalf("DeepStale() = %v, want empty for a document without markers", stale)
	}
}
