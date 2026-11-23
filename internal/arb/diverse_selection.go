package arb

import (
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Each shard preserves global leaders, a bounded reservoir per depth and pool
// representatives. Reducing these reservoirs before final admission keeps serial
// and parallel selection identical, without retaining every positive estimate.
type rankBucket struct {
	profit  rankedHeap
	depth   map[int]rankedHeap
	pools   map[domain.PoolKey]rankedCycle
	diverse bool
}

func (b *rankBucket) admit(candidate rankedCycle, limit int, borrowed bool) {
	keep := len(b.profit) < limit || compareRanked(candidate, b.profit[0]) < 0
	if b.diverse {
		if b.depth == nil {
			b.depth = make(map[int]rankedHeap)
			b.pools = make(map[domain.PoolKey]rankedCycle)
		}
		prior := b.depth[len(candidate.cycle.Pools)]
		keep = keep || len(prior) < depthReservoirLimit(limit) || compareRanked(candidate, prior[0]) < 0
		if !keep {
			for _, key := range candidate.cycle.Pools {
				prior, ok := b.pools[key]
				if !ok || compareRanked(candidate, prior) < 0 {
					keep = true
					break
				}
			}
		}
	}
	if !keep {
		return
	}
	if borrowed {
		candidate = ownRanked(candidate)
	}
	b.profit.admit(candidate, limit)
	if !b.diverse {
		return
	}
	depth := len(candidate.cycle.Pools)
	reservoir := b.depth[depth]
	// members from separate reservoirs overlap during shard reduction.
	if !slices.ContainsFunc(reservoir, func(prior rankedCycle) bool { return sameRankedRoute(prior, candidate) }) {
		reservoir.admit(candidate, depthReservoirLimit(limit))
		b.depth[depth] = reservoir
	}
	for _, key := range candidate.cycle.Pools {
		if prior, ok := b.pools[key]; !ok || compareRanked(candidate, prior) < 0 {
			b.pools[key] = candidate
		}
	}
}
func sameRankedRoute(a, b rankedCycle) bool {
	return a.cycle.Start == b.cycle.Start && slices.Equal(a.cycle.Pools, b.cycle.Pools)
}
func (b *rankBucket) members() []rankedCycle {
	out := slices.Clone([]rankedCycle(b.profit))
	// Pool representatives also contain every depth leader only when it wins a
	// pool, so preserve depth explicitly before the final deterministic reduction.
	for _, candidates := range b.depth {
		out = append(out, candidates...)
	}
	for _, candidate := range b.pools {
		out = append(out, candidate)
	}
	slices.SortFunc(out, compareRanked)
	return slices.CompactFunc(out, sameRankedRoute)
}
func (b *rankBucket) selectCandidates(limit int) []rankedCycle {
	rows := b.members()
	if !b.diverse || len(rows) <= limit {
		return rows[:min(limit, len(rows))]
	}
	// Half the budget always retains the strongest gross estimates. The rest
	// protects missing depths and pools against many variants of one price gap.
	selected := slices.Clone(rows[:(limit+1)/2])
	used := make([]bool, len(rows))
	for i := range selected {
		used[i] = true
	}
	pools := map[domain.PoolKey]bool{}
	cover := func(c rankedCycle) {
		for _, key := range c.cycle.Pools {
			pools[key] = true
		}
	}
	for _, c := range selected {
		cover(c)
	}
	// Reserve a bounded quarter for depth diversity before pool coverage.
	// A single short-path representative is insufficient when many long
	// synthetic estimates describe the same price gap.
	depthSlots := min(limit-len(selected), max(1, limit/4))
	for round := 1; round <= depthReservoirLimit(limit) && depthSlots > 0; round++ {
		for depth := 2; depth <= 16 && depthSlots > 0; depth++ {
			count := 0
			for _, c := range selected {
				if len(c.cycle.Pools) == depth {
					count++
				}
			}
			if count >= round {
				continue
			}
			for i, c := range rows {
				if used[i] || len(c.cycle.Pools) != depth {
					continue
				}
				selected = append(selected, c)
				used[i] = true
				cover(c)
				depthSlots--
				break
			}
		}
	}
	for len(selected) < limit {
		best, unseen := -1, -1
		for i, c := range rows {
			if used[i] {
				continue
			}
			score := 0
			for _, key := range c.cycle.Pools {
				if !pools[key] {
					score++
				}
			}
			if score > unseen {
				best, unseen = i, score
			}
		}
		if best < 0 {
			break
		}
		selected = append(selected, rows[best])
		used[best] = true
		cover(rows[best])
	}
	slices.SortFunc(selected, compareRanked)
	return selected
}

// A route can be skipped only if its ceiling loses to every reservoir it could
// enter. Strict comparison preserves deterministic admission on equal profits.
func (b *rankBucket) rejectUpper(upper *big.Int, layout *compiledPaths, hops []uint32, limit int) bool {
	if len(b.profit) < limit || upper.Cmp(b.profit[0].net) >= 0 {
		return false
	}
	if !b.diverse {
		return true
	}
	depth := b.depth[len(hops)]
	if len(depth) < depthReservoirLimit(limit) || upper.Cmp(depth[0].net) >= 0 {
		return false
	}
	for _, hop := range hops {
		prior, ok := b.pools[layout.keys[hop/2]]
		if !ok || upper.Cmp(prior.net) >= 0 {
			return false
		}
	}
	return true
}

func depthReservoirLimit(limit int) int { return max(1, limit/4) }
