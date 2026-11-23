package arb

import (
	"hash/maphash"
	"math/bits"
)

// rankedRouteHash locates a route by its start and pools. Equal routes hash
// equally; callers compare the full identity among routes sharing a hash.
func rankedRouteHash(seed maphash.Seed, candidate rankedCycle) uint64 {
	hash := maphash.Comparable(seed, candidate.cycle.Start) ^ uint64(len(candidate.cycle.Pools))
	for _, key := range candidate.cycle.Pools {
		hash = bits.RotateLeft64(hash, 13) ^ maphash.Comparable(seed, key)
	}
	return hash
}

// routeSet answers whether a route was added before, comparing only the routes
// that share its hash. Scanning every earlier route made admitting a frame's
// candidates quadratic: about half the search lane's own CPU on a complete
// multi-million-route table.
type routeSet struct {
	seed   maphash.Seed
	byHash map[uint64][]rankedCycle
}

func (s *routeSet) contains(candidate rankedCycle, same func(a, b rankedCycle) bool) bool {
	if s.byHash == nil {
		return false
	}
	for _, old := range s.byHash[rankedRouteHash(s.seed, candidate)] {
		if same(old, candidate) {
			return true
		}
	}
	return false
}

func (s *routeSet) add(candidate rankedCycle) {
	if s.byHash == nil {
		s.seed, s.byHash = maphash.MakeSeed(), make(map[uint64][]rankedCycle)
	}
	hash := rankedRouteHash(s.seed, candidate)
	s.byHash[hash] = append(s.byHash[hash], candidate)
}
