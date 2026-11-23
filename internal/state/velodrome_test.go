package state

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestVelodromeSnapshotDeltaAndRollback(t *testing.T) {
	s := New()
	p := pool.State{Key: domain.PoolKey{Address: testAddress(1), Kind: domain.PoolKindVelodromeV2}, Token0: testAddress(2), Token1: testAddress(3), Fee: 500, Reserve0: big.NewInt(123), Reserve1: big.NewInt(456), Velodrome: &pool.VelodromeState{Factory: testAddress(4), Stable: true, Scale0: big.NewInt(1_000_000), Scale1: big.NewInt(1_000_000_000_000_000_000)}}
	head := domain.BlockRef{Number: 10, Hash: domain.Hash{1}}
	if e := s.BeginBlock(head); e != nil {
		t.Fatal(e)
	}
	if e := s.Upsert(p); e != nil {
		t.Fatal(e)
	}
	s.Commit()
	p.Velodrome.Scale0.SetInt64(10)
	raw, e := s.EncodeSnapshot()
	if e != nil {
		t.Fatal(e)
	}
	var wire snapshotWire
	if e = json.Unmarshal(raw, &wire); e != nil {
		t.Fatal(e)
	}
	if wire.Version != ProtocolStateVersion {
		t.Fatal("old readers could drop stable metadata")
	}
	restored := New()
	if e = restored.LoadSnapshot(raw); e != nil {
		t.Fatal(e)
	}
	r, _ := restored.Pool(p.Key)
	if r.Velodrome.Scale0.Int64() != 1_000_000 || !r.Velodrome.Stable {
		t.Fatal("metadata lost or aliased")
	}
	for _, mutate := range []func(*snapshotWire){func(w *snapshotWire) { w.Version = DirectionalStateVersion }, func(w *snapshotWire) { w.Pools[0].Velodrome = nil }, func(w *snapshotWire) { w.Pools[0].Velodrome.Scale0 = "11" }} {
		var bad snapshotWire
		json.Unmarshal(raw, &bad)
		mutate(&bad)
		b, _ := json.Marshal(bad)
		if e = New().LoadSnapshot(b); e == nil {
			t.Fatal("incomplete protocol state accepted")
		}
	}
	next := domain.BlockRef{Number: 11, Hash: domain.Hash{2}, ParentHash: head.Hash}
	if e = s.BeginBlock(next); e != nil {
		t.Fatal(e)
	}
	r.Fee = 0
	r.Reserve0.SetInt64(789)
	if e = s.Upsert(r); e != nil {
		t.Fatal(e)
	}
	got, _ := restored.Pool(p.Key)
	got.Velodrome.Scale1.SetInt64(1)
	again, _ := restored.Pool(p.Key)
	if again.Velodrome.Scale1.Int64() == 1 {
		t.Fatal("metadata getter aliased")
	}
	if e = s.Rollback(); e != nil {
		t.Fatal(e)
	}
	original, _ := s.Pool(p.Key)
	if original.Fee != 500 || original.Reserve0.Int64() != 123 {
		t.Fatal("rollback lost original")
	}
}
