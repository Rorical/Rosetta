package arb

// interleaveAssets preserves rank within each funding asset. Rank reduction
// sorts assets contiguously; round-robin admission prevents one asset's K
// expensive paths from delaying every other denomination's first result.
func interleaveAssets(candidates []rankedCycle) []rankedCycle {
	if len(candidates) < 2 {
		return candidates
	}
	type span struct{ next, end int }
	groups := []span{{end: len(candidates)}}
	for index := 1; index < len(candidates); index++ {
		if candidates[index].cycle.Start != candidates[index-1].cycle.Start {
			groups[len(groups)-1].end = index
			groups = append(groups, span{next: index, end: len(candidates)})
		}
	}
	if len(groups) == 1 {
		return candidates
	}
	ordered := make([]rankedCycle, 0, len(candidates))
	for len(ordered) < len(candidates) {
		for index := range groups {
			group := &groups[index]
			if group.next < group.end {
				ordered = append(ordered, candidates[group.next])
				group.next++
			}
		}
	}
	return ordered
}
