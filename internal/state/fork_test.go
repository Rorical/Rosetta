package state

import (
	"reflect"
	"sync"
	"testing"
)

func TestStoreForkIndependentWritersAndRollback(t *testing.T) {
	address := testAddress(0x71)
	source := seededStore(t, address)
	key := v3Key(address)
	if err := source.ApplyV3(key, mintEvent(-60, 60, 100), 10); err != nil {
		t.Fatal(err)
	}
	before := source.Checkpoint()
	left, right := source.Fork(), source.Fork()
	for _, fork := range []*Store{left, right} {
		if !reflect.DeepEqual(fork.Checkpoint(), before) || fork.undo != nil || len(fork.history) != 0 {
			t.Fatal("fork did not preserve values and markers without undo history")
		}
	}
	var writers sync.WaitGroup
	for _, fork := range []*Store{left, right} {
		writers.Go(func() {
			for range 20 {
				if err := fork.BeginBlock(blockRef(11)); err != nil {
					t.Error(err)
					return
				}
				if err := fork.ApplyV3(key, mintEvent(-60, 60, 500), 11); err != nil {
					t.Error(err)
					return
				}
				fork.ClearDirty()
				if err := fork.Rollback(); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	writers.Wait()
	for _, store := range []*Store{source, left, right} {
		if !reflect.DeepEqual(store.Checkpoint(), before) {
			t.Fatal("fork mutation or rollback changed a shared snapshot")
		}
	}
}
