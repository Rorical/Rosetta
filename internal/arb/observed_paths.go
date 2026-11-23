package arb

import (
	"bytes"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
)

// observedPathLimit bounds the routes learned from committed receipts. An hour
// of OP receipts typically holds about 820 distinct supported routes, and a
// route rivals reuse can have up to about 600 others between repeats.
const observedPathLimit = 4096

// RememberCycles is called by the canonical state writer. It retains only path
// identities; subsequent searches must quote current state. A separate bounded
// catalog prevents background traversal from evicting recently observed routes.
func (e *Engine) RememberCycles(cycles []graph.Cycle) {
	config := e.exploration
	if config == nil || !config.background {
		return
	}
	previous := config.observed.Load()
	var retained []graph.Cycle
	if previous != nil {
		retained = slices.Clone(previous.cycles)
	}
	changed := false
	for _, cycle := range cycles {
		var valid bool
		cycle, valid = canonicalObservedCycle(config.graph, cycle)
		if !valid {
			continue
		}
		index := slices.IndexFunc(retained, func(old graph.Cycle) bool { return old.Start == cycle.Start && slices.Equal(old.Pools, cycle.Pools) })
		if index >= 0 {
			// A route seen again is the most recent; eviction drops the least
			// recently seen, so routes rivals keep reusing stay.
			if index != len(retained)-1 {
				seen := retained[index]
				retained = append(slices.Delete(retained, index, index+1), seen)
				changed = true
			}
			continue
		}
		if len(retained) == observedPathLimit {
			retained = retained[1:]
		}
		cycle.Pools = slices.Clone(cycle.Pools)
		retained = append(retained, cycle)
		changed = true
	}
	if !changed {
		return
	}
	table, err := graph.BuildTableFromCycles(retained)
	if err != nil {
		return
	} // canonicalObservedCycle validates all external identities.
	config.observed.Store(&pathCatalog{table: table, cycles: retained})
}

// ObservedCycles returns owned identities for a topology replacement.
func (e *Engine) ObservedCycles() []graph.Cycle {
	if e.exploration == nil {
		return nil
	}
	catalog := e.exploration.observed.Load()
	if catalog == nil {
		return nil
	}
	cycles := slices.Clone(catalog.cycles)
	for i := range cycles {
		cycles[i].Pools = slices.Clone(cycles[i].Pools)
	}
	return cycles
}

func canonicalObservedCycle(g *graph.Graph, cycle graph.Cycle) (graph.Cycle, bool) {
	if cycle.Len != len(cycle.Pools) || cycle.Len < 2 || cycle.Len > 16 || cycle.Start == (domain.Address{}) {
		return graph.Cycle{}, false
	}
	token, lowest, rotation := cycle.Start, cycle.Start, 0
	for i, key := range cycle.Pools {
		if bytes.Compare(token[:], lowest[:]) < 0 {
			lowest, rotation = token, i
		}
		if slices.Contains(cycle.Pools[:i], key) {
			return graph.Cycle{}, false
		}
		found := false
		for _, edge := range g.Edges(token) {
			if edge.Pool == key {
				token, found = edge.To, true
				break
			}
		}
		if !found || token == cycle.Start && i+1 != cycle.Len {
			return graph.Cycle{}, false
		}
	}
	if token != cycle.Start {
		return graph.Cycle{}, false
	}
	if rotation != 0 {
		cycle.Pools = append(slices.Clone(cycle.Pools[rotation:]), cycle.Pools[:rotation]...)
		cycle.Start = lowest
	}
	return cycle, true
}
