package arb

import (
	"context"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Build once on the caller, then share read-only across ranking shards. Pool
// readers and fee/funding callbacks need not support concurrent calls within a
// search. Virtual reserves and directional fees are derived once per pool.
type rankSnapshot struct {
	pools   map[domain.PoolKey]*rankPool
	funding map[domain.Address]rankFunding
}

type rankFunding struct {
	bounds InputBounds
	ok     bool
	// valid bounds also carry the float enclosures every rotation's model
	// ceiling needs: low <= Min and high >= Max, converted once per frame.
	valid     bool
	low, high float64
}

var rankSnapshots = sync.Pool{New: func() any {
	return &rankSnapshot{pools: make(map[domain.PoolKey]*rankPool), funding: make(map[domain.Address]rankFunding)}
}}

func (s *rankSnapshot) prepare(ctx context.Context, table CycleTable, indices []int, view View, opts RankedOptions) error {
	work := rankWorkspaces.Get().(*rankWorkspace)
	defer work.release()
	for _, index := range indices {
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, key := range work.cycle(table, index).Pools {
			if _, exists := s.pools[key]; exists {
				continue
			}
			p, ok := view.Pool(key)
			cached := prepareRankPool(p, ok, opts)
			if ok {
				for _, token := range [2]domain.Address{p.Token0, p.Token1} {
					if _, exists := s.funding[token]; !exists {
						s.funding[token] = captureFunding(opts, token)
					}
				}
			}

			s.pools[key] = &cached
		}
	}
	return nil
}

func (s *rankSnapshot) leg(key domain.PoolKey, token domain.Address) (rankLeg, domain.Address, error) {
	p := s.pools[key]
	if p == nil || !p.ok {
		return rankLeg{}, domain.Address{}, &QuoteIssue{Pool: key, Err: ErrMissingPool}
	}
	direction, next := 0, p.state.Token1
	if token == p.state.Token1 {
		direction, next = 1, p.state.Token0
	}
	if err := p.errs[direction]; err != nil {
		return rankLeg{}, next, &QuoteIssue{Pool: key, Err: err}
	}
	return p.legs[direction], next, nil
}

func (s *rankSnapshot) release() {
	oversized := len(s.pools) > 4096 || len(s.funding) > 4096
	clear(s.pools)
	clear(s.funding)
	if !oversized {
		rankSnapshots.Put(s)
	}
}
