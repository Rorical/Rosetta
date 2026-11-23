package pipeline

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/poolrisk"
	"github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
	"math/big"
	"testing"
)

func TestRiskInvalidatesExistingQuotesAndRebuiltTopology(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	pools := []pool.State{
		{Key: domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}, Token0: a, Token1: b, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(2000000)},
		{Key: domain.PoolKey{Address: domain.Address{4}, Kind: domain.PoolKindV2}, Token0: a, Token1: b, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(1000000)},
	}
	store := state.New()
	for _, p := range pools {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	risks, err := tokenrisk.Open("", domain.ChainIdentity{Family: domain.FamilyNitro, ChainID: 42161})
	if err != nil {
		t.Fatal(err)
	}
	defer risks.Close()
	opts := TopologyOptions{TokenCheck: risks.Check, Depth: 2, MaxCycles: 100, K: 4, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }}
	topology, err := BuildTopology(t.Context(), pools, opts)
	if err != nil {
		t.Fatal(err)
	}
	cycle := graph.Cycle{Start: a, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}
	if _, err = topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(1000)); err != nil {
		t.Fatal(err)
	}
	if err = risks.Record(tokenrisk.Evidence{Token: b, Requested: big.NewInt(1000), Received: big.NewInt(990), Block: domain.BlockRef{Hash: domain.Hash{1}}, Source: "executor_trace"}); err != nil {
		t.Fatal(err)
	}
	if _, err = topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(1000)); err == nil {
		t.Fatal("old topology quotes excluded token")
	}
	if _, err = RiskRanking(risks.Check, nil)(pools[0]); err == nil {
		t.Fatal("coarse ranking accepted excluded pool")
	}
	if got := filterRiskPools(pools, risks.Check, nil); len(got) != 0 {
		t.Fatal("blocked pool enters graph")
	}
}

func TestPoolQuarantineRemovesOnlyThatPoolFromQuotesRankingAndGraph(t *testing.T) {
	a, b := domain.Address{1}, domain.Address{2}
	pools := []pool.State{
		{Key: domain.PoolKey{Address: domain.Address{3}, Kind: domain.PoolKindV2}, Token0: a, Token1: b, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(2000000)},
		{Key: domain.PoolKey{Address: domain.Address{4}, Kind: domain.PoolKindV2}, Token0: a, Token1: b, Reserve0: big.NewInt(1000000), Reserve1: big.NewInt(1000000)},
	}
	store := state.New()
	for _, p := range pools {
		if err := store.Upsert(p); err != nil {
			t.Fatal(err)
		}
	}
	quarantine, err := poolrisk.Open("", domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10})
	if err != nil {
		t.Fatal(err)
	}
	defer quarantine.Close()
	opts := TopologyOptions{PoolCheck: quarantine.Check, Depth: 2, MaxCycles: 100, K: 4, Notional: func(domain.Address) *big.Int { return big.NewInt(1000) }}
	topology, err := BuildTopology(t.Context(), pools, opts)
	if err != nil {
		t.Fatal(err)
	}
	cycle := graph.Cycle{Start: a, Pools: []domain.PoolKey{pools[0].Key, pools[1].Key}, Len: 2}
	if _, err = topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(1000)); err != nil {
		t.Fatal(err)
	}
	if err = quarantine.Record(poolrisk.Evidence{Pool: pools[1].Key, Reverts: 3, Routes: 2, Block: domain.BlockRef{Hash: domain.Hash{1}}, Source: "execution_paper"}); err != nil {
		t.Fatal(err)
	}
	if _, err = topology.Engine.QuoteCycle(t.Context(), store.View(), cycle, big.NewInt(1000)); err == nil {
		t.Fatal("existing topology quotes a quarantined pool")
	}
	rank := RiskRanking(nil, quarantine.Check)
	if _, err = rank(pools[1]); err == nil {
		t.Fatal("coarse ranking accepted quarantined pool")
	}
	if _, err = rank(pools[0]); err != nil {
		t.Fatal("quarantine spread to a healthy pool", err)
	}
	if got := filterRiskPools(pools, nil, quarantine.Check); len(got) != 1 || got[0].Key != pools[0].Key {
		t.Fatal("graph did not drop exactly the quarantined pool", got)
	}
}
