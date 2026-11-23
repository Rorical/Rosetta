package arb

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

func TestProgressiveAdmissionReservesBoundedLateProfitLeaders(t *testing.T) {
	var progress progressQuotes
	candidate := func(id byte, profit int64) rankedCycle {
		return rankedCycle{cycle: graph.Cycle{Start: domain.Address{1}, Pools: []domain.PoolKey{{Address: domain.Address{id}}, {Address: domain.Address{255}}}, Len: 2}, net: big.NewInt(profit)}
	}
	for i := byte(1); i <= 4; i++ {
		if len(progress.admit(RankedSelection{candidates: []rankedCycle{candidate(i, int64(i))}}, 2, 4)) != 1 {
			t.Fatal("initial budget lost")
		}
	}
	if got := progress.admit(RankedSelection{candidates: []rankedCycle{candidate(5, 1000)}}, 2, 4); len(got) != 1 {
		t.Fatal("early small routes blocked later high-profit leader")
	}
	if got := progress.admit(RankedSelection{candidates: []rankedCycle{candidate(6, 10000)}}, 2, 4); len(got) != 0 {
		t.Fatal("leader reserve grew without bound")
	}
}
