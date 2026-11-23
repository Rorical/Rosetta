package graph

import (
	"slices"
	"sort"
)

// A sorted hash/ID array costs eight bytes per path. Hash collisions are
// resolved against the actual directed path; they never suppress a new cycle.
func (p *PathLayout) buildMembership() {
	p.members = make([]uint64, len(p.Paths))
	for index := range p.Paths {
		path := p.Path(index)
		first := path[0]
		hash := uint32(2166136261) ^ p.Pairs[first/2][first%2]
		for _, hop := range path {
			hash = (hash ^ hop) * 16777619
		}
		p.members[index] = uint64(hash)<<32 | uint64(index)
	}
	slices.Sort(p.members)
}

func (p *PathLayout) contains(cycle Cycle) bool {
	if len(cycle.Pools) < 2 {
		return false
	}
	first, ok := p.poolIDs[cycle.Pools[0]]
	if !ok {
		return false
	}
	pair := p.Pairs[first]
	start := pair[0]
	if p.Tokens[start] != cycle.Start {
		start = pair[1]
		if p.Tokens[start] != cycle.Start {
			return false
		}
	}
	hash := uint32(2166136261) ^ start
	current := cycle.Start
	for _, key := range cycle.Pools {
		id, ok := p.poolIDs[key]
		if !ok {
			return false
		}
		pair := p.Pairs[id]
		direction := uint32(0)
		switch current {
		case p.Tokens[pair[0]]:
			current = p.Tokens[pair[1]]
		case p.Tokens[pair[1]]:
			direction, current = 1, p.Tokens[pair[0]]
		default:
			return false
		}
		hash = (hash ^ (id*2 + direction)) * 16777619
	}
	if current != cycle.Start {
		return false
	}
	lower := uint64(hash) << 32
	offset := sort.Search(len(p.members), func(i int) bool { return p.members[i] >= lower })
	for ; offset < len(p.members) && uint32(p.members[offset]>>32) == hash; offset++ {
		path := p.Path(int(uint32(p.members[offset])))
		if len(path) != len(cycle.Pools) {
			continue
		}
		first := path[0]
		if p.Tokens[p.Pairs[first/2][first%2]] != cycle.Start {
			continue
		}
		equal := true
		for i, hop := range path {
			if p.Keys[hop/2] != cycle.Pools[i] {
				equal = false
				break
			}
		}
		if equal {
			return true
		}
	}
	return false
}

func (t *Table) contains(cycle Cycle) bool {
	if t.packed != nil {
		return t.packed.contains(cycle)
	}
	// The explorer normally receives a packed runtime table. The generic path
	// supports small standalone fixtures without a second global membership map.
	for _, index := range t.byPool[cycle.Pools[0]] {
		existing := t.cycles[index]
		if existing.Start == cycle.Start && slices.Equal(existing.Pools, cycle.Pools) {
			return true
		}
	}
	return false
}

// ContainsCycle expects the canonical token rotation used by this table.
func (t *Table) ContainsCycle(cycle Cycle) bool { return len(cycle.Pools) > 0 && t.contains(cycle) }
