package arb

import (
	"context"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

type pathCatalog struct {
	table     *graph.Table
	cycles    []graph.Cycle
	truncated bool
	// budget and cacheFull are the causes of truncated.
	budget, cacheFull bool
}

// EnableBackgroundExploration separates topology traversal from live price
// updates. One worker-owned traversal covers quiet markets too. Both source
// lanes read immutable postings and only rank paths affected by dirty pools.
func (e *Engine) EnableBackgroundExploration(g *graph.Graph, limits graph.ExploreLimits) error {
	if err := e.EnableExploration(g, limits); err != nil {
		return err
	}
	e.exploration.background = true
	return nil
}

func (c *rankCache) catalogPaths(ctx context.Context, config *explorationConfig, dirty []domain.PoolKey) (graph.Exploration, error) {
	result := graph.Exploration{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	catalog, observed := config.catalog.Load(), config.observed.Load()
	c.catalogCycles = c.catalogCycles[:0]
	if observed != nil {
		for _, id := range c.observedIDs.Select(observed.table, dirty, observed.table.Len()) {
			c.catalogCycles = append(c.catalogCycles, observed.cycles[id])
		}
		result.CatalogPaths += observed.table.Len()
	}
	if catalog == nil {
		result.BudgetExhausted = true
	} else {
		for _, id := range c.catalogSelection.Select(catalog.table, dirty, catalog.table.Len()) {
			cycle := catalog.cycles[id]
			if observed == nil || !observed.table.ContainsCycle(cycle) {
				c.catalogCycles = append(c.catalogCycles, cycle)
			}
		}
		result.CatalogPaths += catalog.table.Len()
		result.BudgetExhausted, result.CacheTruncated = catalog.budget, catalog.cacheFull
	}
	result.Cycles = c.catalogCycles
	return result, nil
}

func (w *Worker) maintainPaths(ctx context.Context) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var owner *Engine
	var explorer *graph.Explorer
	var keys []domain.PoolKey
	var publishAfter time.Time
	var changed, complete bool
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		w.mu.Lock()
		engine, cpus := w.engine, w.cpus
		w.mu.Unlock()
		if engine.exploration == nil || !engine.exploration.background {
			continue
		}
		if engine == owner && complete {
			continue
		}
		// Maintenance yields when live work occupies the entire CPU budget.
		// A quantum has fixed edge/path limits, so it cannot hold a core while
		// waiting for a reader, RPC, publication or the next timer tick.
		lease, ok := cpus.tryAcquire()
		if !ok {
			continue
		}
		if engine != owner {
			owner = engine
			changed, complete, publishAfter = false, false, time.Time{}
			config := engine.exploration
			explorer, _ = graph.NewExplorer(config.graph, config.table, config.limits)
			keys = config.graph.PoolKeys()
		}
		result, err := explorer.Explore(ctx, keys)
		truncated := result.BudgetExhausted || result.CacheTruncated
		changed = changed || result.Added > 0
		complete = err == nil && !truncated
		previous := engine.exploration.catalog.Load()
		if err == nil && (previous == nil || previous.truncated != truncated || changed && !time.Now().Before(publishAfter)) {
			engine.exploration.catalog.Store(&pathCatalog{table: result.Indexed(), cycles: result.Cycles, truncated: truncated, budget: result.BudgetExhausted, cacheFull: result.CacheTruncated})
			changed, publishAfter = false, time.Now().Add(500*time.Millisecond)
		}
		lease.release()
	}
}
