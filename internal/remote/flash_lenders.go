package remote

import (
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// flashLenders lists the pools that may fund a V3 flash loan. Funding reads
// only a lender's key, fee and tokens, which never change for a pool, so the
// list is rebuilt only when the pool set changes (graph version or count),
// instead of copying every pool on the main loop for each admitted offer.
func (r *runtime) flashLenders() []pool.State {
	var graph uint64 // a runtime without a search scheduler never replaces its graph
	if r.search != nil {
		graph = r.search.GraphVersion
	}
	if r.lenders.pools != nil && r.lenders.graph == graph && r.lenders.count == r.store.Len() {
		return r.lenders.pools
	}
	pools := []pool.State{}
	for _, p := range r.store.Pools() {
		if p.Key.Kind == domain.PoolKindV3 && p.Fee < 1_000_000 {
			pools = append(pools, p)
		}
	}
	r.lenders.pools, r.lenders.graph, r.lenders.count = pools, graph, r.store.Len()
	return pools
}
