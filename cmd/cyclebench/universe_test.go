package main

import (
	"context"
	"fmt"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/simulate"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestBuildRingsShape(t *testing.T) {
	for _, depth := range []int{9, 10, 12, 16} {
		t.Run(fmt.Sprintf("depth_%d", depth), func(t *testing.T) {
			const ringCount = 3
			rings, err := buildRings(depth, ringCount)
			if err != nil {
				t.Fatalf("buildRings: %v", err)
			}
			if len(rings) != ringCount {
				t.Fatalf("rings = %d, want %d", len(rings), ringCount)
			}

			tokens := make(map[domain.Address]struct{}, depth*ringCount)
			pools := make(map[domain.PoolKey]struct{}, depth*ringCount)
			for r, ring := range rings {
				if len(ring.tokens) != depth {
					t.Fatalf("ring %d tokens = %d, want %d", r, len(ring.tokens), depth)
				}
				if len(ring.pools) != depth {
					t.Fatalf("ring %d pools = %d, want %d", r, len(ring.pools), depth)
				}
				for i := 0; i < depth; i++ {
					token := ring.tokens[i]
					if token == (domain.Address{}) {
						t.Fatalf("ring %d token %d is the zero address", r, i)
					}
					if _, exists := tokens[token]; exists {
						t.Fatalf("ring %d token %d %s repeats across rings", r, i, token.Hex())
					}
					tokens[token] = struct{}{}

					shallow := ring.pools[i]
					if shallow.Key.Address == (domain.Address{}) {
						t.Fatalf("ring %d pool %d has the zero address", r, i)
					}
					if _, exists := pools[shallow.Key]; exists {
						t.Fatalf("ring %d pool %d key repeats", r, i)
					}
					pools[shallow.Key] = struct{}{}
					if shallow.Key.Kind != domain.PoolKindV2 {
						t.Errorf("ring %d pool %d kind = %d, want V2", r, i, shallow.Key.Kind)
					}
					if shallow.Token0 != token || shallow.Token1 != ring.tokens[(i+1)%depth] {
						t.Errorf("ring %d pool %d token pair is not the ring edge", r, i)
					}
					if shallow.Fee != feePips {
						t.Errorf("ring %d pool %d fee = %d, want %d", r, i, shallow.Fee, feePips)
					}
					if shallow.Reserve0 == nil || shallow.Reserve1 == nil ||
						shallow.Reserve0.Sign() <= 0 || shallow.Reserve1.Sign() <= 0 {
						t.Errorf("ring %d pool %d has non-positive reserves", r, i)
					}
				}
			}

			if got, want := rings[0].pools[0].Reserve0, newReserve(1_200_000); got.Cmp(want) != 0 {
				t.Errorf("imbalanced reserve0 = %s, want %s", got, want)
			}
			if got, want := rings[0].pools[0].Reserve1, newReserve(800_000); got.Cmp(want) != 0 {
				t.Errorf("imbalanced reserve1 = %s, want %s", got, want)
			}
			for r, ring := range rings {
				for i, shallow := range ring.pools {
					if r == 0 && i == 0 {
						continue
					}
					if got, want := shallow.Reserve0, newReserve(1_000_000); got.Cmp(want) != 0 {
						t.Errorf("ring %d pool %d reserve0 = %s, want %s", r, i, got, want)
					}
					if got, want := shallow.Reserve1, newReserve(1_000_000); got.Cmp(want) != 0 {
						t.Errorf("ring %d pool %d reserve1 = %s, want %s", r, i, got, want)
					}
				}
			}

			again, err := buildRings(depth, ringCount)
			if err != nil {
				t.Fatalf("second buildRings: %v", err)
			}
			for r := range rings {
				for i := range rings[r].tokens {
					if rings[r].tokens[i] != again[r].tokens[i] {
						t.Errorf("ring %d token %d is not deterministic", r, i)
					}
					if rings[r].pools[i].Key != again[r].pools[i].Key {
						t.Errorf("ring %d pool %d is not deterministic", r, i)
					}
				}
			}
		})
	}
}

func TestKnownCycleUsesProfitableDirection(t *testing.T) {
	rings, err := buildRings(10, 2)
	if err != nil {
		t.Fatalf("buildRings: %v", err)
	}
	ring := rings[0]
	known := knownCycle(ring)

	if known.Len != len(ring.pools) || len(known.Pools) != len(ring.pools) {
		t.Fatalf("known cycle len = %d/%d, want %d", known.Len, len(known.Pools), len(ring.pools))
	}

	smallest := ring.tokens[0]
	for _, token := range ring.tokens[1:] {
		if addressLess(token, smallest) {
			smallest = token
		}
	}
	if known.Start != smallest {
		t.Fatalf("known start = %s, want smallest token %s", known.Start.Hex(), smallest.Hex())
	}

	byKey := make(map[domain.PoolKey]pool.State, len(ring.pools))
	for _, shallow := range ring.pools {
		byKey[shallow.Key] = shallow
	}

	seen := make(map[domain.PoolKey]struct{}, len(known.Pools))
	current := known.Start
	for _, key := range known.Pools {
		shallow, ok := byKey[key]
		if !ok {
			t.Fatalf("cycle uses pool %s outside the ring", key.Address.Hex())
		}
		if _, duplicate := seen[key]; duplicate {
			t.Fatalf("cycle uses pool %s twice", key.Address.Hex())
		}
		seen[key] = struct{}{}

		enteredWith := current
		switch current {
		case shallow.Token0:
			current = shallow.Token1
		case shallow.Token1:
			current = shallow.Token0
		default:
			t.Fatalf("leg %s does not connect at %s", key.Address.Hex(), current.Hex())
		}
		if key == ring.pools[0].Key && enteredWith != shallow.Token1 {
			t.Fatalf("imbalanced pool entered with %s, want Token1 %s",
				enteredWith.Hex(), shallow.Token1.Hex())
		}
	}
	if len(seen) != len(ring.pools) {
		t.Fatalf("cycle pools = %d, want %d", len(seen), len(ring.pools))
	}
	if current != known.Start {
		t.Fatalf("cycle ends at %s, want start %s", current.Hex(), known.Start.Hex())
	}
}

func TestSyntheticUniverseFindsProfitableCycle(t *testing.T) {
	for depth := minDepth; depth <= maxDepth; depth++ {
		t.Run(fmt.Sprintf("depth_%d", depth), func(t *testing.T) {
			const ringCount = 2
			universe, err := newUniverse(depth, ringCount)
			if err != nil {
				t.Fatalf("newUniverse: %v", err)
			}

			built, err := graph.Build(universe.pools)
			if err != nil {
				t.Fatalf("build graph: %v", err)
			}
			table, err := graph.BuildDeepTableRange(built, depth, depth, 8, 8)
			if err != nil {
				t.Fatalf("build deep table: %v", err)
			}
			if want := 2 * ringCount; table.Len() != want {
				t.Fatalf("table cycles = %d, want %d (two directions per ring)", table.Len(), want)
			}
			if index := findTableCycle(table, universe.known); index < 0 {
				t.Fatal("known cycle is missing from the deep table")
			}

			store := state.New()
			for _, shallow := range universe.pools {
				if err := store.Upsert(shallow); err != nil {
					t.Fatalf("upsert pool %s: %v", shallow.Key.Address.Hex(), err)
				}
			}
			quoter, err := simulate.NewQuoter(func(shallow pool.State) (pool.Adapter, error) {
				return uniswapv2.New(shallow.Fee)
			})
			if err != nil {
				t.Fatalf("build quoter: %v", err)
			}
			engine, err := arb.New(table, func(domain.Address) *big.Int {
				return big.NewInt(tokenUnit)
			}, 200, quoter)
			if err != nil {
				t.Fatalf("build engine: %v", err)
			}

			opportunities, err := engine.Evaluate(context.Background(), store, store.Dirty())
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			rank := findRank(opportunities, universe.known)
			if rank != 1 {
				t.Fatalf("known cycle rank = %d, want 1 (opportunities %d)", rank, len(opportunities))
			}
			if got := opportunities[0].Net.Sign(); got <= 0 {
				t.Fatalf("known net = %s, want positive", opportunities[0].Net)
			}

			positive := 0
			for _, opportunity := range opportunities {
				if opportunity.Net.Sign() > 0 {
					positive++
				}
			}
			if positive != 1 {
				t.Fatalf("profitable cycles = %d, want exactly 1", positive)
			}
		})
	}
}

// findTableCycle returns the table index of target, or -1 when absent.
func findTableCycle(table *graph.DeepTable, target graph.Cycle) int {
	for i := 0; i < table.Len(); i++ {
		if sameDirectedCycle(table.Cycle(i), target) {
			return i
		}
	}
	return -1
}
