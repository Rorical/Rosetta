package graph

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func testAddress(id byte) domain.Address {
	var address domain.Address
	address[len(address)-1] = id
	return address
}

func testPool(poolID, token0, token1 byte) pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: testAddress(poolID),
			Kind:    domain.PoolKindV2,
		},
		Token0: testAddress(token0),
		Token1: testAddress(token1),
	}
}

func mustBuild(t *testing.T, pools ...pool.State) *Graph {
	t.Helper()
	g, err := Build(pools)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	return g
}

func mustBuildTable(t *testing.T, g *Graph, maxDepth, maxCycles int) *Table {
	t.Helper()
	table, err := BuildTable(g, maxDepth, maxCycles)
	if err != nil {
		t.Fatalf("BuildTable: %v", err)
	}
	return table
}
