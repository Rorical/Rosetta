package ingest

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestV4RoutingBindsPoolIDToItsRegisteredManager(t *testing.T) {
	first := domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{1}, PoolID: domain.Hash{1}}
	second := domain.PoolKey{Kind: domain.PoolKindV4, Address: domain.Address{2}, PoolID: domain.Hash{2}}
	routes, err := newRouting([]domain.PoolKey{first, second})
	if err != nil {
		t.Fatal(err)
	}
	log := Log{Address: first.Address, Topics: []domain.Hash{{9}, second.PoolID}}
	if _, tracked := routes.resolveKey(log); tracked {
		t.Fatal("a log at one manager updated another manager's pool")
	}
	log.Address = second.Address
	if key, tracked := routes.resolveKey(log); !tracked || key != second {
		t.Fatal("registered manager and pool ID did not route")
	}
}
