package graph

import (
	"errors"
	"slices"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestBuildAddsBothDirections(t *testing.T) {
	poolA := testPool(0x10, 0x01, 0x02)
	poolB := testPool(0x20, 0x01, 0x02)
	g := mustBuild(t, poolA, poolB)

	if got := g.PoolCount(); got != 2 {
		t.Fatalf("PoolCount() = %d, want 2", got)
	}

	tokenA, tokenB := testAddress(0x01), testAddress(0x02)
	wantFromA := []Edge{
		{Pool: poolA.Key, From: tokenA, To: tokenB},
		{Pool: poolB.Key, From: tokenA, To: tokenB},
	}
	if got := g.Edges(tokenA); !slices.Equal(got, wantFromA) {
		t.Fatalf("Edges(A) = %+v, want %+v", got, wantFromA)
	}

	wantFromB := []Edge{
		{Pool: poolA.Key, From: tokenB, To: tokenA},
		{Pool: poolB.Key, From: tokenB, To: tokenA},
	}
	if got := g.Edges(tokenB); !slices.Equal(got, wantFromB) {
		t.Fatalf("Edges(B) = %+v, want %+v", got, wantFromB)
	}

	if got := g.Edges(testAddress(0x03)); got != nil {
		t.Fatalf("Edges(unknown) = %+v, want nil", got)
	}
}

func TestEdgesReturnsCopy(t *testing.T) {
	g := mustBuild(t, testPool(0x10, 0x01, 0x02))
	edges := g.Edges(testAddress(0x01))
	edges[0].To = testAddress(0x09)

	if got := g.Edges(testAddress(0x01))[0].To; got != testAddress(0x02) {
		t.Fatalf("mutating Edges result changed the graph: To = %s", got)
	}
}

func TestBuildOrderIndependent(t *testing.T) {
	pools := []pool.State{
		testPool(0x30, 0x01, 0x02),
		testPool(0x10, 0x02, 0x03),
		testPool(0x20, 0x03, 0x01),
	}
	forward := mustBuild(t, pools...)
	reversedPools := slices.Clone(pools)
	slices.Reverse(reversedPools)
	reversed := mustBuild(t, reversedPools...)

	for _, token := range []domain.Address{testAddress(0x01), testAddress(0x02), testAddress(0x03)} {
		if !slices.Equal(forward.Edges(token), reversed.Edges(token)) {
			t.Fatalf("adjacency for %s depends on input order", token)
		}
	}
}

func TestBuildRejectsInvalidPools(t *testing.T) {
	tests := []struct {
		name  string
		pools []pool.State
		want  error
	}{
		{
			name:  "duplicate address",
			pools: []pool.State{testPool(0x10, 0x01, 0x02), testPool(0x10, 0x02, 0x03)},
			want:  ErrDuplicatePool,
		},
		{
			name:  "identical tokens",
			pools: []pool.State{testPool(0x10, 0x01, 0x01)},
			want:  ErrInvalidPool,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			graph, err := Build(test.pools)
			if !errors.Is(err, test.want) {
				t.Fatalf("Build() error = %v, want %v", err, test.want)
			}
			if graph != nil {
				t.Fatalf("Build() returned a graph with an error")
			}
		})
	}
}

// TestBuildAcceptsV4Siblings checks that two V4 pools behind one PoolManager
// address are distinct graph participants rather than a duplicate pool.
func TestBuildAcceptsV4Siblings(t *testing.T) {
	manager := testAddress(0x40)
	first := v4Pool(manager, 0x01, 0x01, 0x02)
	second := v4Pool(manager, 0x02, 0x02, 0x01)

	g := mustBuild(t, first, second)
	if got := g.PoolCount(); got != 2 {
		t.Fatalf("PoolCount() = %d, want 2", got)
	}
	edges := g.Edges(testAddress(0x01))
	if len(edges) == 0 {
		t.Fatal("Edges(token0) is empty")
	}
	seen := make(map[domain.PoolKey]struct{}, len(edges))
	for _, edge := range edges {
		seen[edge.Pool] = struct{}{}
	}
	if _, ok := seen[first.Key]; !ok {
		t.Fatalf("first V4 pool missing from adjacency: %+v", edges)
	}
	if _, ok := seen[second.Key]; !ok {
		t.Fatalf("second V4 pool missing from adjacency: %+v", edges)
	}

	table := mustBuildTable(t, g, 2, 10)
	if got := len(table.byPool); got != 2 {
		t.Fatalf("table indexes %d pools, want 2", got)
	}
	if got := table.Candidates([]domain.PoolKey{first.Key}, 10); len(got) == 0 {
		t.Fatal("Candidates(first V4 sibling) is empty")
	}
	if got := table.Candidates([]domain.PoolKey{second.Key}, 10); len(got) == 0 {
		t.Fatal("Candidates(second V4 sibling) is empty")
	}

	if _, err := Build([]pool.State{first, first}); !errors.Is(err, ErrDuplicatePool) {
		t.Fatalf("Build(same key twice) error = %v, want ErrDuplicatePool", err)
	}
}

func v4Pool(manager domain.Address, poolID byte, token0, token1 byte) pool.State {
	return pool.State{
		Key:    domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{poolID}},
		Token0: testAddress(token0),
		Token1: testAddress(token1),
	}
}
