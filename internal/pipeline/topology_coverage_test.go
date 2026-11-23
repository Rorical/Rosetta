package pipeline

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestFullTopologyStillAdmitsNewFiveHopMarket(t *testing.T) {
	token := func(n byte) domain.Address { return domain.Address{n} }
	var old []pool.State
	for i := byte(1); i <= 4; i++ {
		old = append(old, pool.State{Key: domain.PoolKey{Address: token(i), Kind: domain.PoolKindV2}, Token0: token(1), Token1: token(2), Reserve0: big.NewInt(1000), Reserve1: big.NewInt(1000)})
	}
	config := TopologyOptions{Depth: 5, MaxCycles: 12, K: 2, Notional: func(domain.Address) *big.Int { return big.NewInt(1) }}
	base, err := BuildTopology(t.Context(), old, config)
	if err != nil {
		t.Fatal(err)
	}
	if base.Table.Len() != config.MaxCycles {
		t.Fatal("fixture did not fill original path budget")
	}
	expanded := append([]pool.State(nil), old...)
	var added []domain.PoolKey
	for i := byte(0); i < 5; i++ {
		from, to := token(10+i), token(11+i)
		if i == 0 {
			from = token(1)
		}
		if i == 4 {
			to = token(1)
		}
		p := pool.State{Key: domain.PoolKey{Address: token(20 + i), Kind: domain.PoolKindV2}, Token0: from, Token1: to, Reserve0: big.NewInt(1000), Reserve1: big.NewInt(1000)}
		expanded = append(expanded, p)
		added = append(added, p.Key)
	}
	check := func(table *graph.Table) {
		t.Helper()
		for _, key := range added {
			ids := table.Candidates([]domain.PoolKey{key}, table.Len())
			if len(ids) == 0 {
				t.Fatal("new pool has no searchable paths")
			}
			for _, id := range ids {
				if table.Cycle(id).Len != 5 {
					t.Fatal("invalid long-tail cycle")
				}
			}
		}
	}
	next, err := ExtendTopology(t.Context(), expanded, base, added, config)
	if err != nil {
		t.Fatal(err)
	}
	if next.Table.Len() > config.MaxCycles || !next.Truncated {
		t.Fatal("lost truncation boundary")
	}
	check(next.Table)
	if len(base.Table.Candidates(added, base.Table.Len())) != 0 {
		t.Fatal("mutated live original table")
	}
	restarted, err := BuildTopology(t.Context(), expanded, config)
	if err != nil {
		t.Fatal(err)
	}
	check(restarted.Table)
}
