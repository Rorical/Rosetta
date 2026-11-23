package state

import (
	"encoding/json"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestDirectionalFeeSnapshotDeltaRollbackAndCopy(t *testing.T) {
	store := New()
	p := v3State(domain.Address{1})
	p.Key.Kind = domain.PoolKindCamelotV3
	reverse := uint32(0)
	p.FeeOneForZero = &reverse
	head := domain.BlockRef{Number: 10, Hash: domain.Hash{1}}
	if err := store.BeginBlock(head); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(p); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	reverse = 9000
	encoded, err := store.EncodeSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	var wire snapshotWire
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Version != DirectionalStateVersion {
		t.Fatal("old readers could silently drop directional fee")
	}
	wire.Version = SnapshotVersion
	legacy, err := json.Marshal(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := New().LoadSnapshot(legacy); err == nil {
		t.Fatal("version 1 accepted independent reverse fee")
	}
	restored := New()
	if err := restored.LoadSnapshot(encoded); err != nil {
		t.Fatal(err)
	}
	read, _ := restored.Pool(p.Key)
	if read.FeeOneForZero == nil || *read.FeeOneForZero != 0 {
		t.Fatal("zero directional fee lost or input aliased")
	}
	*read.FeeOneForZero = 500
	readAgain, _ := restored.Pool(p.Key)
	if *readAgain.FeeOneForZero != 0 {
		t.Fatal("snapshot getter aliased fee")
	}
	next := domain.BlockRef{Number: 11, Hash: domain.Hash{2}, ParentHash: head.Hash}
	if err := store.BeginBlock(next); err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(read); err != nil {
		t.Fatal(err)
	}
	if err := store.Rollback(); err != nil {
		t.Fatal(err)
	}
	got, _ := store.Pool(p.Key)
	if got.FeeOneForZero == nil || *got.FeeOneForZero != 0 {
		t.Fatal("rollback lost original zero fee")
	}
}

func TestInvalidDirectionalMetadataCannotCreateUnreadableSnapshot(t *testing.T) {
	p := v3State(domain.Address{1})
	fee := uint32(500)
	p.FeeOneForZero = &fee
	if err := New().Upsert(p); err == nil {
		t.Fatal("non-Camelot directional metadata accepted")
	}
	p.Key.Kind = domain.PoolKindCamelotV3
	fee = 1_000_000
	if err := New().Upsert(p); err == nil {
		t.Fatal("out-of-range reverse fee accepted")
	}
}
