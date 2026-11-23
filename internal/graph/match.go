package graph

import "github.com/Rorical/Rosetta/internal/domain"

// SameDirectedCycle compares rotations while preserving entry token/direction.
// In particular, the two directions of a parallel two-pool cycle are distinct.
func SameDirectedCycle(a, b Cycle, tokens func(domain.PoolKey) (domain.Address, domain.Address, bool)) bool {
	count := len(a.Pools)
	if count == 0 || count != len(b.Pools) || a.Len != count || b.Len != count {
		return false
	}
	for offset := 0; offset < count; offset++ {
		matches := true
		for i, key := range b.Pools {
			if key != a.Pools[(i+offset)%count] {
				matches = false
				break
			}
		}
		if !matches {
			continue
		}
		current := a.Start
		valid := true
		for i := 0; i < offset; i++ {
			token0, token1, ok := tokens(a.Pools[i])
			if !ok {
				valid = false
				break
			}
			if current == token0 {
				current = token1
			} else if current == token1 {
				current = token0
			} else {
				valid = false
				break
			}
		}
		if valid && current == b.Start {
			return true
		}
	}
	return false
}
