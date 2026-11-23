package graph

import (
	"bytes"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Edge is one directed swap leg: From swapped to To through Pool.
type Edge struct {
	Pool domain.PoolKey
	From domain.Address
	To   domain.Address
}

// Graph is the directed token adjacency graph of a set of pools. Each pool
// contributes one edge per swap direction. A Graph is immutable after Build.
type Graph struct {
	adjacency map[domain.Address][]Edge
	tokens    []domain.Address
	poolCount int

	// Dense mirror of adjacency for cycle enumeration, which would otherwise
	// spend most of its time hashing addresses and pool keys. Token indices
	// follow the sorted tokens slice, so comparing indices compares addresses.
	links    [][]link
	poolKeys []domain.PoolKey
	// poolTokens holds each pool's lower and higher token index.
	poolTokens [][2]int32
	poolIndex  map[domain.PoolKey]int32
	tokenIndex map[domain.Address]int32
}

// link is one directed edge in dense form: a pool index and the token index it
// swaps into.
type link struct {
	pool int32
	to   int32
}

// PoolKeys returns every pool identity once, independent of token direction.
func (g *Graph) PoolKeys() []domain.PoolKey {
	keys := make([]domain.PoolKey, 0, g.poolCount)
	for _, edges := range g.adjacency {
		for _, edge := range edges {
			if compareAddresses(edge.From, edge.To) < 0 {
				keys = append(keys, edge.Pool)
			}
		}
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return keys
}

// Build constructs the token graph for pools. Pool keys must be unique and
// each pool's two tokens must differ. Adjacency lists are sorted by pool key
// and swap direction, so the resulting graph does not depend on input order.
// Two V4 pools behind one PoolManager address are distinct keys and both join
// the graph.
func Build(pools []pool.State) (*Graph, error) {
	g := &Graph{
		adjacency: make(map[domain.Address][]Edge, len(pools)*2),
		poolCount: len(pools),
	}
	seenPools := make(map[domain.PoolKey]struct{}, len(pools))
	tokens := make(map[domain.Address]struct{}, len(pools)*2)
	for _, state := range pools {
		if state.Token0 == state.Token1 {
			return nil, fmt.Errorf("%w: pool %s has identical tokens", ErrInvalidPool, poolKeyLabel(state.Key))
		}
		if _, exists := seenPools[state.Key]; exists {
			return nil, fmt.Errorf("%w: %s", ErrDuplicatePool, poolKeyLabel(state.Key))
		}
		seenPools[state.Key] = struct{}{}
		g.adjacency[state.Token0] = append(g.adjacency[state.Token0], Edge{
			Pool: state.Key,
			From: state.Token0,
			To:   state.Token1,
		})
		g.adjacency[state.Token1] = append(g.adjacency[state.Token1], Edge{
			Pool: state.Key,
			From: state.Token1,
			To:   state.Token0,
		})
		tokens[state.Token0] = struct{}{}
		tokens[state.Token1] = struct{}{}
	}
	for _, edges := range g.adjacency {
		sortEdges(edges)
	}
	g.tokens = make([]domain.Address, 0, len(tokens))
	for token := range tokens {
		g.tokens = append(g.tokens, token)
	}
	slices.SortFunc(g.tokens, compareAddresses)
	g.indexDense()
	return g, nil
}

// indexDense numbers tokens in sorted order and pools in first-seen order of
// that walk, and copies each adjacency list in its sorted edge order.
func (g *Graph) indexDense() {
	g.tokenIndex = make(map[domain.Address]int32, len(g.tokens))
	for i, token := range g.tokens {
		g.tokenIndex[token] = int32(i)
	}
	g.poolIndex = make(map[domain.PoolKey]int32, g.poolCount)
	g.poolKeys = make([]domain.PoolKey, 0, g.poolCount)
	g.links = make([][]link, len(g.tokens))
	for i, token := range g.tokens {
		edges := g.adjacency[token]
		links := make([]link, len(edges))
		for j, edge := range edges {
			id, ok := g.poolIndex[edge.Pool]
			if !ok {
				// Tokens are walked in order, so a pool is first seen from its lower token.
				id = int32(len(g.poolKeys))
				g.poolIndex[edge.Pool] = id
				g.poolKeys = append(g.poolKeys, edge.Pool)
				g.poolTokens = append(g.poolTokens, [2]int32{int32(i), g.tokenIndex[edge.To]})
			}
			links[j] = link{pool: id, to: g.tokenIndex[edge.To]}
		}
		g.links[i] = links
	}
}

// anchorMask marks the dense indices of keys; keys outside the graph are
// ignored.
func (g *Graph) anchorMask(keys []domain.PoolKey) []bool {
	mask := make([]bool, len(g.poolKeys))
	for _, key := range keys {
		if id, ok := g.poolIndex[key]; ok {
			mask[id] = true
		}
	}
	return mask
}

// Edges returns the outgoing edges of from in deterministic order. The result
// is a copy and may be modified by the caller.
func (g *Graph) Edges(from domain.Address) []Edge {
	return slices.Clone(g.adjacency[from])
}

// PoolCount returns the number of pools used to build the graph.
func (g *Graph) PoolCount() int {
	return g.poolCount
}

// sortEdges orders by pool key and then swap direction. A pool contributes at
// most one edge to a given adjacency list, but From/To keep the order total.
func sortEdges(edges []Edge) {
	slices.SortFunc(edges, func(a, b Edge) int {
		if diff := domain.ComparePoolKeys(a.Pool, b.Pool); diff != 0 {
			return diff
		}
		if diff := compareAddresses(a.From, b.From); diff != 0 {
			return diff
		}
		return compareAddresses(a.To, b.To)
	})
}

// poolKeyLabel renders a key for an error message. The pool ID is part of the
// name when it is set, because V4 pools share the PoolManager address.
func poolKeyLabel(key domain.PoolKey) string {
	if key.PoolID == (domain.Hash{}) {
		return key.Address.Hex()
	}
	return key.Address.Hex() + "/" + key.PoolID.Hex()
}

func compareAddresses(a, b domain.Address) int {
	return bytes.Compare(a[:], b[:])
}
