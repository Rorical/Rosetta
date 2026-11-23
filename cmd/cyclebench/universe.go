package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Supported cycle lengths. graph.BuildDeepTableRange supports 2..16; the lower
// bound keeps the benchmark on genuinely long cycles.
const (
	minDepth = 9
	maxDepth = 16
)

// feePips is 0.30%, the fee common to every synthetic pool.
const feePips uint32 = 30

// tokenUnit is 1e18 base units, one 18-decimal token.
const tokenUnit int64 = 1_000_000_000_000_000_000

// Synthetic address classes keep token addresses disjoint from pool addresses.
const (
	tokenAddressClass byte = 0x11
	poolAddressClass  byte = 0x22
)

// ring is one disjoint cycle: pool i trades tokens[i] against tokens[i+1],
// wrapping at the end.
type ring struct {
	tokens []domain.Address
	pools  []pool.State
}

// syntheticUniverse is the generated pool set plus the one directed cycle that
// is expected to be profitable.
type syntheticUniverse struct {
	pools []pool.State
	known graph.Cycle
}

// newUniverse returns rings disjoint rings of depth V2 pools each. Every ring
// trades a private token set, so its only simple cycles are the two directions
// around the ring. Ring 0's first pool is imbalanced while every other pool is
// balanced: one direction of ring 0 can profit, and every other cycle only pays
// fees.
func newUniverse(depth, rings int) (*syntheticUniverse, error) {
	built, err := buildRings(depth, rings)
	if err != nil {
		return nil, err
	}
	pools := make([]pool.State, 0, depth*rings)
	for _, r := range built {
		pools = append(pools, r.pools...)
	}
	return &syntheticUniverse{pools: pools, known: knownCycle(built[0])}, nil
}

// buildRings constructs the synthetic rings without touching a store or graph.
func buildRings(depth, rings int) ([]ring, error) {
	if depth < minDepth || depth > maxDepth {
		return nil, fmt.Errorf("depth %d: want %d..%d", depth, minDepth, maxDepth)
	}
	if rings < 1 {
		return nil, fmt.Errorf("rings %d: want positive", rings)
	}

	built := make([]ring, 0, rings)
	for r := 0; r < rings; r++ {
		tokens := make([]domain.Address, depth)
		for i := range tokens {
			tokens[i] = syntheticAddress(tokenAddressClass, r, i)
		}

		pools := make([]pool.State, depth)
		for i := range pools {
			reserve0 := newReserve(1_000_000)
			reserve1 := newReserve(1_000_000)
			if r == 0 && i == 0 {
				reserve0 = newReserve(1_200_000)
				reserve1 = newReserve(800_000)
			}
			pools[i] = pool.State{
				Key: domain.PoolKey{
					Address: syntheticAddress(poolAddressClass, r, i),
					Kind:    domain.PoolKindV2,
				},
				Token0:   tokens[i],
				Token1:   tokens[(i+1)%depth],
				Fee:      feePips,
				Reserve0: reserve0,
				Reserve1: reserve1,
			}
		}
		built = append(built, ring{tokens: tokens, pools: pools})
	}
	return built, nil
}

// syntheticAddress builds a deterministic address. The first byte names the
// class, so tokens and pools never collide. The permuted second byte makes
// address order differ from traversal order within a ring, so the table's
// smallest-token rotation is exercised rather than always landing on index 0.
// The remaining bytes encode ring and item, keeping every address unique.
func syntheticAddress(class byte, ringIndex, itemIndex int) domain.Address {
	var addr domain.Address
	addr[0] = class
	addr[1] = byte((itemIndex*5 + ringIndex*3) % 16)
	binary.BigEndian.PutUint32(addr[2:6], uint32(ringIndex))
	binary.BigEndian.PutUint32(addr[6:10], uint32(itemIndex+1))
	return addr
}

// knownCycle returns ring 0's profitable directed cycle in the form the deep
// table stores: starting at the ring's smallest token. The imbalanced pool
// holds more Token0 than Token1, so the profitable direction enters it with
// Token1 and leaves with Token0. That is the direction of decreasing ring
// index through the ring.
func knownCycle(r ring) graph.Cycle {
	start := 0
	for i := range r.tokens {
		if addressLess(r.tokens[i], r.tokens[start]) {
			start = i
		}
	}

	pools := make([]domain.PoolKey, len(r.pools))
	for step := 1; step <= len(r.pools); step++ {
		index := (start - step + len(r.pools)) % len(r.pools)
		pools[step-1] = r.pools[index].Key
	}
	return graph.Cycle{Pools: pools, Start: r.tokens[start], Len: len(pools)}
}

// addressLess orders addresses the way graph orders its start tokens.
func addressLess(a, b domain.Address) bool {
	return bytes.Compare(a[:], b[:]) < 0
}

// newReserve returns tokens*1e18 as a fresh big.Int, so seeded pools never
// share mutable reserves.
func newReserve(tokens int64) *big.Int {
	return new(big.Int).Mul(big.NewInt(tokens), big.NewInt(tokenUnit))
}
