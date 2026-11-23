package arb

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"math/big"
	"testing"
)

func TestDiverseSelectionKeepsMoreThanOneShortRouteAgainstLongVariants(t *testing.T) {
	b := &rankBucket{diverse: true}
	makeCandidate := func(id byte, depth int, profit int64) rankedCycle {
		keys := make([]domain.PoolKey, depth)
		for i := range keys {
			keys[i] = domain.PoolKey{Address: domain.Address{byte(i + 1)}}
		}
		keys[0].Address[1] = id
		return rankedCycle{cycle: graph.Cycle{Start: domain.Address{1}, Pools: keys, Len: depth}, net: big.NewInt(profit)}
	}
	for i := byte(1); i <= 40; i++ {
		b.admit(makeCandidate(i, 6, 10000-int64(i)), 32, false)
	}
	first, second := makeCandidate(50, 3, 100), makeCandidate(51, 3, 90)
	b.admit(first, 32, false)
	b.admit(second, 32, false)
	rows := b.selectCandidates(32)
	found := false
	for _, c := range rows {
		if sameRankedRoute(c, second) {
			found = true
		}
	}
	if !found {
		t.Fatal("single depth leader hid next short opportunity")
	}
	if len(rows) != 32 {
		t.Fatal("selection budget changed", len(rows))
	}
}
