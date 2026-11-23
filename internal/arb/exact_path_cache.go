package arb

import (
	"hash/maphash"
	"math/big"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
)

type exactPathResult struct {
	candidate rankedCycle
	job       SearchJob
	quote     Opportunity
	ok        bool
}

// Each lane owns a bounded cache of reader-free numerical results. Failed
// dependency quotes are not cached, so loading a wider window can retry them.
type exactPathCache struct {
	once    sync.Once
	seed    maphash.Seed
	index   map[uint64][]int
	mu      sync.RWMutex
	entries []exactPathResult
	next    int
}

func (c *exactPathCache) initialize() {
	c.once.Do(func() { c.seed = maphash.MakeSeed(); c.index = make(map[uint64][]int) })
}

func (c *exactPathCache) fingerprint(candidate rankedCycle) uint64 {
	return rankedRouteHash(c.seed, candidate)
}

func sameExactInput(a, b rankedCycle) bool {
	return a.cycle.Start == b.cycle.Start && slices.Equal(a.cycle.Pools, b.cycle.Pools) &&
		a.bounds.Min.Cmp(b.bounds.Min) == 0 && a.bounds.Max.Cmp(b.bounds.Max) == 0
}

func (c *exactPathCache) find(candidate rankedCycle, valid func(SearchJob, []domain.PoolKey) bool) (exactPathResult, bool) {
	c.initialize()
	hash := c.fingerprint(candidate)
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, index := range c.index[hash] {
		entry := c.entries[index]
		// Hashes only locate entries. Full identity and revisions prove reuse.
		if sameExactInput(entry.candidate, candidate) && valid(entry.job, candidate.cycle.Pools) {
			return entry, true
		}
	}
	return exactPathResult{}, false
}

func (c *exactPathCache) store(candidate rankedCycle, job SearchJob, quote Opportunity, ok bool) {
	c.initialize()
	hash := c.fingerprint(candidate)
	// Keep identity and revision only. No View, closures, receipts or readers.
	identity := SearchJob{Version: job.Version, GraphVersion: job.GraphVersion, ticket: job.ticket, generation: job.generation}
	owned := rankedCycle{cycle: candidate.cycle, bounds: InputBounds{Min: new(big.Int).Set(candidate.bounds.Min), Max: new(big.Int).Set(candidate.bounds.Max), Floor: candidate.bounds.Floor}}
	owned.cycle.Pools = slices.Clone(candidate.cycle.Pools)
	entry := exactPathResult{candidate: owned, job: identity, ok: ok}
	if ok && quote.Net.Sign() > 0 {
		entry.quote = copyOpportunity(quote)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, index := range c.index[hash] {
		if sameExactInput(c.entries[index].candidate, candidate) {
			c.entries[index] = entry
			return
		}
	}
	index := len(c.entries)
	if index < 512 {
		c.entries = append(c.entries, entry)
	} else {
		index = c.next
		oldHash := c.fingerprint(c.entries[index].candidate)
		bucket := c.index[oldHash]
		for offset, value := range bucket {
			if value == index {
				bucket = slices.Delete(bucket, offset, offset+1)
				break
			}
		}
		if len(bucket) == 0 {
			delete(c.index, oldHash)
		} else {
			c.index[oldHash] = bucket
		}
		c.entries[index] = entry
		c.next = (c.next + 1) % len(c.entries)
	}
	c.index[hash] = append(c.index[hash], index)
}

func copyOpportunity(source Opportunity) Opportunity {
	clone := func(value *big.Int) *big.Int {
		if value == nil {
			return nil
		}
		return new(big.Int).Set(value)
	}
	out := source
	out.Cycle.Pools = slices.Clone(source.Cycle.Pools)
	out.Input, out.Output, out.Net = clone(source.Input), clone(source.Output), clone(source.Net)
	out.GasCost, out.NetAfterGas = clone(source.GasCost), clone(source.NetAfterGas)
	out.Legs = make([]Leg, len(source.Legs))
	for i, leg := range source.Legs {
		out.Legs[i] = Leg{Pool: leg.Pool, AmountIn: clone(leg.AmountIn), AmountOut: clone(leg.AmountOut)}
	}
	return out
}
