package discovery

import (
	"context"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// staleSelections bounds how long an unread entry survives. Discovery selects
// twice per run and the recheck sees only loaded additions, so an entry must
// outlive one recheck to be reused by the next run's candidate pass.
const staleSelections = 4

// MarketSelector is SelectMarketsWithPinned with cycle enumeration reused
// between calls. The zero value is ready. It is not safe for concurrent use;
// one discovery worker owns it and resets it with its scan generation.
type MarketSelector struct {
	cycles cycleCache
}

// Select returns exactly what SelectMarketsWithPinned returns for the same
// arguments; only candidates whose cycles may have changed are re-enumerated.
func (s *MarketSelector) Select(ctx context.Context, pools []pool.State, activity map[domain.PoolKey]Activity, policy SelectionPolicy, maxPools, depth int, pinned []domain.PoolKey) (Selection, error) {
	return selectMarkets(ctx, &s.cycles, pools, activity, policy, maxPools, depth, pinned)
}

// Reset drops every cached enumeration.
func (s *MarketSelector) Reset() {
	s.cycles = cycleCache{}
}

// cycleCache holds each candidate's anchored enumerations. An entry is a pure
// function of the anchor, the depth, and the set of cycles through the anchor
// in the pool graph, so it stays valid until the graph gains or loses a pool
// that can lie on such a cycle. Reserves and activity never enter the graph;
// they change it only by moving pools in or out of eligibility, which shows up
// here as an added or removed pool.
type cycleCache struct {
	depth   int
	round   uint64
	pools   map[domain.PoolKey]tokenPair // the eligible graph entries are valid for
	entries map[domain.PoolKey]*anchorEntry
}

type tokenPair [2]domain.Address

// anchorEntry stores one more cycle than each budget when the walk found that
// many. The extra cycle witnesses truncation: removing a pool none of the
// stored cycles use leaves both the budgeted prefix and the overflow intact.
// A list without the witness is complete.
type anchorEntry struct {
	tokens      tokenPair
	short, deep []graph.Cycle
	read        uint64
}

// prune updates the entry for a graph without the removed pools and reports
// whether it is still exact. A complete list loses exactly the cycles that use
// a removed pool and keeps its order. A truncated list must stay untouched:
// once one of its cycles disappears, the cycle that would replace it is
// unknown.
func (e *anchorEntry) prune(removed map[domain.PoolKey]bool, anchor domain.PoolKey) bool {
	gone := func(cycle graph.Cycle) bool {
		return slices.ContainsFunc(cycle.Pools, func(key domain.PoolKey) bool { return key != anchor && removed[key] })
	}
	short, ok := pruneCycles(e.short, candidateShortCycles, gone)
	if !ok {
		return false
	}
	deep, ok := pruneCycles(e.deep, candidateDeepCycles, gone)
	if !ok {
		return false
	}
	e.short, e.deep = short, deep
	return true
}

// pruneCycles copies before deleting: earlier selections may still hold the
// old slice.
func pruneCycles(cycles []graph.Cycle, budget int, gone func(graph.Cycle) bool) ([]graph.Cycle, bool) {
	if !slices.ContainsFunc(cycles, gone) {
		return cycles, true
	}
	if len(cycles) > budget {
		return nil, false
	}
	return slices.DeleteFunc(slices.Clone(cycles), gone), true
}

// sync moves the cache to the eligible graph, pruning or dropping every entry
// the change can affect. Map iteration order only decides which independent
// entry is examined first, never what any entry holds.
func (c *cycleCache) sync(eligible []pool.State, depth int) {
	c.round++
	next := make(map[domain.PoolKey]tokenPair, len(eligible))
	for _, p := range eligible {
		next[p.Key] = tokenPair{p.Token0, p.Token1}
	}
	previous := c.pools
	c.pools = next
	if previous == nil || depth != c.depth {
		c.depth, c.entries = depth, map[domain.PoolKey]*anchorEntry{}
		return
	}
	var added []pool.State
	for _, p := range eligible {
		if tokens, ok := previous[p.Key]; !ok || tokens != next[p.Key] {
			added = append(added, p)
		}
	}
	removed := map[domain.PoolKey]bool{}
	for key, tokens := range previous {
		if now, ok := next[key]; !ok || now != tokens {
			removed[key] = true
		}
	}
	var reach *additionReach
	if len(added) > 0 {
		reach = newAdditionReach(eligible, added, depth)
	}
	for anchor, entry := range c.entries {
		if c.round-entry.read > staleSelections || reach != nil && reach.touches(anchor, entry.tokens) || !entry.prune(removed, anchor) {
			delete(c.entries, anchor)
		}
	}
}

// anchored returns the candidate's short and deep enumerations, from the
// cache when the entry is still valid. A nil cache always enumerates.
func (c *cycleCache) anchored(ctx context.Context, g *graph.Graph, anchor domain.PoolKey, depth int) (*anchorEntry, error) {
	var tokens tokenPair
	if c != nil {
		tokens = c.pools[anchor]
		if entry := c.entries[anchor]; entry != nil && entry.tokens == tokens {
			entry.read = c.round
			return entry, nil
		}
	}
	entry := &anchorEntry{tokens: tokens}
	var err error
	if entry.short, _, err = graph.AnchoredCycles(ctx, g, anchor, 2, candidateShortCycles+1); err != nil {
		return nil, err
	}
	if depth > 2 {
		if entry.deep, _, err = graph.AnchoredCycles(ctx, g, anchor, depth, candidateDeepCycles+1); err != nil {
			return nil, err
		}
	}
	if c != nil {
		entry.read = c.round
		c.entries[anchor] = entry
	}
	return entry, nil
}

// budgeted is the first budget cycles and whether more exist. The walk order
// is fixed, so this equals an enumeration with the budget itself.
func budgeted(cycles []graph.Cycle, budget int) ([]graph.Cycle, bool) {
	if len(cycles) > budget {
		return cycles[:budget], true
	}
	return cycles, false
}
