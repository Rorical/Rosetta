package graph

import (
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestCycleIntoReusesStorageWithoutExposingTable(t *testing.T) {
	cycles := []Cycle{{Start: domain.Address{1}, Pools: []domain.PoolKey{{Address: domain.Address{2}}, {Address: domain.Address{3}}}, Len: 2}}
	table, err := BuildTableFromCycles(cycles)
	if err != nil {
		t.Fatal(err)
	}
	merged, err := NewMergedTable([]CycleSource{table})
	if err != nil {
		t.Fatal(err)
	}
	deep := &DeepTable{cycles: table.cycles}
	for _, source := range []interface {
		Cycle(int) Cycle
		CycleInto(int, []domain.PoolKey) Cycle
	}{table, deep, merged} {
		dst := make([]domain.PoolKey, 0, 16)
		copied := source.CycleInto(0, dst)
		if !reflect.DeepEqual(copied, source.Cycle(0)) || &copied.Pools[0] != &dst[:1][0] {
			t.Fatal("cycle copy failed to reuse caller storage")
		}
		copied.Pools[0] = domain.PoolKey{}
		if !reflect.DeepEqual(source.Cycle(0), cycles[0]) {
			t.Fatal("caller changed immutable table through a copied cycle")
		}
		if source.CycleInto(-1, dst).Pools != nil || source.CycleInto(1, dst).Pools != nil {
			t.Fatal("out-of-range cycle must stay empty")
		}
	}
}
