package arb

import (
	"context"
	"fmt"
	"slices"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

type explorationConfig struct {
	graph      *graph.Graph
	table      *graph.Table
	limits     graph.ExploreLimits
	background bool
	catalog    atomic.Pointer[pathCatalog]
	observed   atomic.Pointer[pathCatalog]
}

type ExplorationStats struct {
	Steps, Discovered, Cached int
	CatalogPaths              int
	Truncated                 bool
	// BudgetExhausted and CacheFull split Truncated by cause.
	BudgetExhausted, CacheFull bool
}

// EnableExploration configures lane-owned bounded searches beyond the indexed
// table. Call before publishing the engine. It does not change the main table.
func (e *Engine) EnableExploration(g *graph.Graph, limits graph.ExploreLimits) error {
	table, ok := e.table.(*graph.Table)
	if !ok {
		return fmt.Errorf("%w: exploration requires runtime table", ErrInvalidConfig)
	}
	if _, err := graph.NewExplorer(g, table, limits); err != nil {
		return err
	}
	e.exploration = &explorationConfig{graph: g, table: table, limits: limits}
	return nil
}

func (c *rankCache) explore(ctx context.Context, engine *Engine, dirty []domain.PoolKey) (graph.Exploration, error) {
	if engine.exploration == nil {
		return graph.Exploration{}, nil
	}
	if engine.exploration.background {
		return c.catalogPaths(ctx, engine.exploration, dirty)
	}
	if c.explorer == nil || c.explorerEngine != engine {
		config := engine.exploration
		explorer, err := graph.NewExplorer(config.graph, config.table, config.limits)
		if err != nil {
			return graph.Exploration{}, err
		}
		c.explorer, c.explorerEngine = explorer, engine
	}
	return c.explorer.Explore(ctx, dirty)
}

// cyclePools returns the sorted, distinct union of dirty and the cycles' pools.
// Catalog paths repeat a few hundred pools many times over, so deduplicating
// through the lane's reused set first keeps the sort to the distinct pools.
func (c *rankCache) cyclePools(dirty []domain.PoolKey, cycles []graph.Cycle) []domain.PoolKey {
	if c.poolSet == nil {
		c.poolSet = make(map[domain.PoolKey]struct{})
	}
	defer clear(c.poolSet)
	var keys []domain.PoolKey
	for _, key := range dirty {
		if _, seen := c.poolSet[key]; !seen {
			c.poolSet[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	for _, cycle := range cycles {
		for _, key := range cycle.Pools {
			if _, seen := c.poolSet[key]; !seen {
				c.poolSet[key] = struct{}{}
				keys = append(keys, key)
			}
		}
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return keys
}

func (e *Engine) rankExplored(ctx context.Context, view View, cycles []graph.Cycle, dirty []domain.PoolKey, opts RankedOptions, workers int) (RankedSelection, error) {
	if len(cycles) == 0 {
		return RankedSelection{}, nil
	}
	// Cached paths stay in compact, bounded lane storage. A temporary source
	// borrows their immutable identities instead of cloning another path table.
	extra, err := New(exploredCycles(cycles), e.notional, e.K(), e.quoter)
	if err != nil {
		return RankedSelection{}, err
	}
	return extra.rankWithWorkers(ctx, view, dirty, opts, nil, workers)
}

type exploredCycles []graph.Cycle

func (c exploredCycles) Len() int                    { return len(c) }
func (c exploredCycles) Cycle(index int) graph.Cycle { return c.CycleInto(index, nil) }
func (c exploredCycles) CycleInto(index int, dst []domain.PoolKey) graph.Cycle {
	if index < 0 || index >= len(c) {
		return graph.Cycle{}
	}
	cycle := c[index]
	cycle.Pools = append(dst[:0], cycle.Pools...)
	return cycle
}
func (c exploredCycles) Candidates(dirty []domain.PoolKey, k int) []int {
	if len(dirty) == 0 || k < 1 {
		return nil
	}
	var out []int
	for i, cycle := range c {
		for _, key := range cycle.Pools {
			if slices.Contains(dirty, key) {
				out = append(out, i)
				break
			}
		}
	}
	return out
}

func mergeRanked(limit int, parts ...RankedSelection) RankedSelection {
	return mergeRankedDiverse(limit, false, parts...)
}

func mergeRankedDiverse(limit int, diverse bool, parts ...RankedSelection) RankedSelection {
	result := RankedSelection{}
	ranked := make(map[domain.Address]*rankBucket)
	for _, part := range parts {
		result.considered += part.considered
		for _, issue := range part.issues {
			result.issues = appendIssue(result.issues, issue)
		}
		for _, candidate := range part.candidates {
			bucket := ranked[candidate.cycle.Start]
			if bucket == nil {
				bucket = &rankBucket{diverse: diverse}
				ranked[candidate.cycle.Start] = bucket
			}
			duplicate := false
			for _, prior := range bucket.profit {
				if slices.Equal(prior.cycle.Pools, candidate.cycle.Pools) {
					duplicate = true
					break
				}
			}
			if !duplicate {
				bucket.admit(candidate, limit, false)
			}
		}
	}
	for _, bucket := range ranked {
		result.candidates = append(result.candidates, bucket.selectCandidates(limit)...)
	}
	slices.SortFunc(result.candidates, func(a, b rankedCycle) int {
		if order := slices.Compare(a.cycle.Start[:], b.cycle.Start[:]); order != 0 {
			return order
		}
		return compareRanked(a, b)
	})
	return result
}
