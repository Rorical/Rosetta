package state

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// UpdateMetric applies update to every MetricOmm pool priced by oracle, or
// held to it as its reference, and
// marks it changed in block. The pools are independent of each other, so the
// map's iteration order does not reach the result.
func (s *Store) UpdateMetric(oracle domain.Address, block uint64, update func(*pool.MetricState)) {
	s.ensureMaps()
	for key, current := range s.pools {
		if current.Metric == nil || current.Metric.Oracle != oracle && current.Metric.Reference != oracle {
			continue
		}
		next := current.Copy()
		update(next.Metric)
		next.UpdatedBlock = block
		s.recordUndo(key)
		s.pools[key] = next
		s.dirty[key] = struct{}{}
	}
}
