package main

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// poolTokens resolves a pool's token pair. The rotation check uses it to derive
// which token a rotated cycle would start at.
type poolTokens func(domain.PoolKey) (token0, token1 domain.Address, ok bool)

// sameCycle reports whether a and b traverse the same directed sequence of
// pools, allowing b to begin at a different point of the loop.
//
// The pool sequence alone is not enough: two pools over the same token pair
// admit a cycle in both directions, and rotating a two-pool cycle by one looks
// like the reversed sequence. The rotated entry token must therefore equal
// b.Start.
func sameCycle(a, b graph.Cycle, tokens poolTokens) bool {
	return graph.SameDirectedCycle(a, b, tokens)
}

// diffBps returns (got-want) as a fraction of want, in basis points. It returns
// nil when want is zero and the ratio is undefined.
func diffBps(got, want *big.Int) *big.Rat {
	if want.Sign() == 0 {
		return nil
	}
	scaled := new(big.Int).Sub(got, want)
	scaled.Mul(scaled, big.NewInt(10_000))
	return new(big.Rat).SetFrac(scaled, want)
}

func formatBps(ratio *big.Rat) string {
	if ratio == nil {
		return "na"
	}
	return ratio.FloatString(4)
}
