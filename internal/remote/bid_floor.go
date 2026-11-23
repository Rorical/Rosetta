package remote

import (
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

const (
	// contestFloorWindows contested windows on a route's pools are needed
	// before their tips bound ours from below.
	contestFloorWindows = 3
	// contestFloorAge is how far back those windows count.
	contestFloorAge = 6 * time.Hour
	// contestFloorShare caps the floor as a share of the opportunity's worth:
	// following contests that paid more than half of it leaves too little.
	contestFloorShareNum, contestFloorShareDen = 1, 2
)

// freeLossShare is the least share of an opportunity's worth bid as the tip
// when a lost race is free (every pool held by a condition). It falls as the
// surplus, in wei of the native asset, grows: 5 in 10 under 2.5e13 (about
// $0.05), 4 in 10 under 1.5e14 (about $0.30) and 3 in 10 above.
//
// On opportunities a rival filled on the same route, the rival's tip
// typically takes a median 73% of a surplus under $0.02 and 34% of one
// between $0.10 and $0.30. Replayed against such fills, these shares win
// about 40% more races than a flat 3 in 10 and keep slightly more. The
// ceiling still bounds the tip.
func freeLossShare(surplus *big.Int) (num, den uint64) {
	switch {
	case surplus.Cmp(freeLossSmall) < 0:
		return 5, 10
	case surplus.Cmp(freeLossMedium) < 0:
		return 4, 10
	}
	return 3, 10
}

var (
	freeLossSmall  = big.NewInt(25_000_000_000_000)
	freeLossMedium = big.NewInt(150_000_000_000_000)
)

// contestFloor is the tip below which a bid on route is not worth making:
// the median share of an opportunity's worth that the highest rival tip took
// in recent contested windows on the route's pools, applied to worth. 0 when
// fewer than contestFloorWindows such windows are known. The caller holds mu.
//
// Rivals can pay 9–39% of the gross as tip on opportunities where the window
// model bids under 1% at a predicted win of a third.
func (e *bidEstimator) contestFloor(now time.Time, route []domain.PoolKey, worth uint64) uint64 {
	var shares []float64
	for i := len(e.windows) - 1; i >= 0; i-- {
		w := &e.windows[i]
		if now.Sub(w.start) > contestFloorAge {
			break
		}
		if w.worth == 0 || len(w.contest) == 0 || !overlaps(w.pools, route) {
			continue
		}
		var top uint64
		for _, c := range w.contest {
			top = max(top, c.tip)
		}
		// A rival that paid more than the opportunity was worth to us bids
		// above value (a larger size or a cheaper executor): it is not
		// followed, and the model's predatory-rival rule decides.
		if top >= w.worth {
			continue
		}
		shares = append(shares, float64(top)/float64(w.worth))
	}
	if len(shares) < contestFloorWindows {
		return 0
	}
	slices.Sort(shares)
	floor := uint64(shares[len(shares)/2] * float64(worth))
	return min(floor, worth/contestFloorShareDen*contestFloorShareNum)
}
