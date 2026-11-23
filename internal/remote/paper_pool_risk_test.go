package remote

import (
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/poolrisk"
	"github.com/Rorical/Rosetta/internal/tokenrisk"
)

func TestPaperRevertsQuarantinePoolsUnlessTokenIsAlreadyExcluded(t *testing.T) {
	store, pools := poolFixture(t)
	chain := domain.ChainIdentity{Family: domain.FamilyOPStack, ChainID: 10}
	tokens, err := tokenrisk.Open("", chain)
	if err != nil {
		t.Fatal(err)
	}
	quarantine, err := poolrisk.Open("", chain)
	if err != nil {
		t.Fatal(err)
	}
	p := &executionPaper{risks: tokens, pools: quarantine}
	job := func(order ...int) paperExecutionJob {
		cycle := graph.Cycle{Start: pools[0].Token0, Len: len(order)}
		for _, i := range order {
			cycle.Pools = append(cycle.Pools, pools[i].Key)
		}
		return paperExecutionJob{event: arb.SearchEvent{Job: arb.SearchJob{View: store.View()}}, opportunity: arb.Opportunity{Cycle: cycle, Net: big.NewInt(1)}}
	}
	result := func(status, basis string) paperExecutionResult {
		return paperExecutionResult{Status: status, StateBasis: basis, Version: domain.StateVersion{Anchor: domain.BlockRef{Number: 10, Hash: domain.Hash{10}}}}
	}
	observe := func(j paperExecutionJob, r paperExecutionResult) {
		if err := p.observePools(j, r); err != nil {
			t.Fatal(err)
		}
	}
	observe(job(0, 1), result("reverted", "discovery_snapshot"))
	observe(job(0, 1), result("reverted", "later_observed_prefix"))
	observe(job(0, 1), result("rejected_fees", "discovery_snapshot"))
	observe(job(0, 1), result("reverted", "discovery_snapshot"))
	if quarantine.Check(pools[0].Key) != nil {
		t.Fatal("quarantined before a second route failed")
	}
	observe(job(1, 0), result("reverted", "discovery_snapshot"))
	for _, pool := range pools {
		if !errors.Is(quarantine.Check(pool.Key), poolrisk.ErrQuarantined) {
			t.Fatal("pool on three reverted routes was not quarantined", pool.Key)
		}
	}

	quarantine, err = poolrisk.Open("", chain)
	if err != nil {
		t.Fatal(err)
	}
	p = &executionPaper{risks: tokens, pools: quarantine}
	if err := tokens.Record(tokenrisk.Evidence{Token: pools[0].Token1, Requested: big.NewInt(10), Received: big.NewInt(9), Block: domain.BlockRef{Hash: domain.Hash{1}}, Source: "executor_trace"}); err != nil {
		t.Fatal(err)
	}
	for _, order := range [][]int{{0, 1}, {1, 0}, {0, 1}} {
		observe(job(order...), result("reverted", "discovery_snapshot"))
	}
	if quarantine.Check(pools[0].Key) != nil {
		t.Fatal("token shortfall reverts were blamed on the pools")
	}
}
