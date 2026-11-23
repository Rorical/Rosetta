package remote

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// A route is sent only once its quote state holds the whole block after the
// one where it first quoted a profit; a route that went away starts over.
func TestLongTailSendsOnlyRoutesThatSurvivedTheNextBlock(t *testing.T) {
	route := arb.Opportunity{Cycle: graph.Cycle{Start: domain.Address{1}, Pools: []domain.PoolKey{{Address: domain.Address{2}}, {Address: domain.Address{3}}}}, Net: big.NewInt(5)}
	rotated := route
	rotated.Cycle = graph.Cycle{Start: domain.Address{4}, Pools: []domain.PoolKey{{Address: domain.Address{3}}, {Address: domain.Address{2}}}}
	var tail longTail
	tail.observe([]arb.Opportunity{route}, 100)
	if len(tail.survivors([]arb.Opportunity{route}, 99)) != 0 || len(tail.survivors([]arb.Opportunity{route}, 100)) != 0 {
		t.Fatal("sent before the next block was in its quote state")
	}
	tail.observe([]arb.Opportunity{route}, 101)
	if len(tail.survivors([]arb.Opportunity{rotated}, 101)) != 1 {
		t.Fatal("survivor reported from another pool was not sent")
	}
	tail.observe([]arb.Opportunity{route}, 120) // unseen for 19 blocks: a new run
	if len(tail.survivors([]arb.Opportunity{route}, 120)) != 0 {
		t.Fatal("a returning route counted its earlier run")
	}
	loss := route
	loss.Cycle.Pools = []domain.PoolKey{{Address: domain.Address{8}}, {Address: domain.Address{9}}}
	loss.Net = big.NewInt(-1)
	tail.observe([]arb.Opportunity{loss}, 130)
	if len(tail.survivors([]arb.Opportunity{loss}, 140)) != 0 {
		t.Fatal("a losing quote started a run")
	}
}
