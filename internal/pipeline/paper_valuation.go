package pipeline

import (
	"math/big"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

// NativeValue values a positive token amount at the selected view's spot prices.
// The result rounds down in wei. It is a valuation scenario, not a swap quote.
// No RPC is made; outside-route paths reuse the canonical conversion inventory.
func (r *Research) NativeValue(view arb.View, cycle graph.Cycle, wrapped domain.Address, amount *big.Int) (*big.Int, string, []domain.PoolKey) {
	if amount == nil || amount.Sign() < 0 {
		return nil, "unavailable", nil
	}
	if cycle.Start == wrapped || cycle.Start == (domain.Address{}) {
		return new(big.Int).Set(amount), "native_1_to_1", nil
	}
	// The route's own pools after the wrapped native price it best: they are
	// the pools the trade moves. A tail that cannot be priced (a MetricOmm
	// pool without a quote) falls back to the start asset's stored path.
	if keys := nativeConversionTail(view, cycle, wrapped); len(keys) != 0 {
		if value := valueAlong(view, keys, wrapped, cycle.Start, amount, nil); value != nil {
			return value, "route_spot_scenario", slices.Clone(keys)
		}
	}
	var keys []domain.PoolKey
	// built is the bottleneck reserve a cached spot path had when it was
	// chosen; the path is refused once its pools hold under half of it.
	var built *big.Int
	if r != nil && r.book != nil {
		r.book.mu.RLock()
		keys = slices.Clone(r.book.conversions[cycle.Start].keys)
		r.book.mu.RUnlock()
	} else if r != nil {
		r.spot.mu.RLock()
		if r.spot.wrapped == wrapped {
			keys = slices.Clone(r.spot.paths[cycle.Start].keys)
			built = r.spot.paths[cycle.Start].capacity
		}
		r.spot.mu.RUnlock()
	}
	if len(keys) == 0 {
		return nil, "unavailable", nil
	}
	value := valueAlong(view, keys, wrapped, cycle.Start, amount, built)
	if value == nil {
		return nil, "unavailable", nil
	}
	return value, "local_pool_spot_scenario", keys
}

// valueAlong converts amount of start into the wrapped native at the spot
// prices of keys, which lead from the wrapped native to start; nil when a
// pool is missing or has no price, the path does not end at start, or a pool
// holds under half of built.
func valueAlong(view arb.View, keys []domain.PoolKey, wrapped, start domain.Address, amount, built *big.Int) *big.Int {
	rate := big.NewRat(1, 1)
	token := wrapped
	for _, key := range keys {
		p, ok := view.Pool(key)
		if !ok {
			return nil
		}
		n, d, ok := paperSpot(p, token)
		if !ok {
			return nil
		}
		if built != nil {
			// The paths are rebuilt every spotRefreshBlocks. A pool drained
			// since then prices the token cheaply for whoever moves it.
			r0, r1, ok := pool.SpotReserves(p)
			if !ok {
				return nil
			}
			reserve := r0
			if token == p.Token1 {
				reserve = r1
			}
			capacity := new(big.Int).Mul(reserve, rate.Denom())
			capacity.Quo(capacity, rate.Num())
			if capacity.Lsh(capacity, 1).Cmp(built) < 0 {
				return nil
			}
		}
		rate.Mul(rate, new(big.Rat).SetFrac(n, d))
		if token == p.Token0 {
			token = p.Token1
		} else {
			token = p.Token0
		}
	}
	if token != start {
		return nil
	}
	value := new(big.Int).Mul(amount, rate.Denom())
	return value.Quo(value, rate.Num())
}

func nativeConversionTail(view arb.View, cycle graph.Cycle, wrapped domain.Address) []domain.PoolKey {
	token, start := cycle.Start, -1
	for i, key := range cycle.Pools {
		if token == wrapped {
			start = i
		}
		p, ok := view.Pool(key)
		if !ok {
			return nil
		}
		if token == p.Token0 {
			token = p.Token1
		} else if token == p.Token1 {
			token = p.Token0
		} else {
			return nil
		}
	}
	if token != cycle.Start || start < 0 {
		return nil
	}
	return cycle.Pools[start:]
}

// spotConversions prices start assets in the wrapped native for a chain that
// runs without a paper book. Only the pools of each path are kept; their
// prices are read from the quote view at use.
type spotConversions struct {
	mu      sync.RWMutex
	wrapped domain.Address
	paths   map[domain.Address]conversionPath
	block   uint64 // the block the paths were built at
}

// spotRefreshBlocks is how often the paths are rebuilt: which pool prices a
// token changes slowly, and the build walks every pool three times.
const spotRefreshBlocks = 240

// SetWrappedNative names the wrapped native that NativeValue converts to when
// no paper book does it.
func (r *Research) SetWrappedNative(wrapped domain.Address) {
	if r == nil {
		return
	}
	r.spot.mu.Lock()
	defer r.spot.mu.Unlock()
	if r.spot.wrapped != wrapped {
		r.spot.wrapped, r.spot.paths, r.spot.block = wrapped, nil, 0
	}
}

// observeSpot rebuilds the conversion paths from a canonical view.
func (r *Research) observeSpot(base poolstate.View, ref domain.BlockRef) {
	r.spot.mu.RLock()
	wrapped, due := r.spot.wrapped, r.spot.paths == nil || ref.Number >= r.spot.block+spotRefreshBlocks || ref.Number < r.spot.block
	r.spot.mu.RUnlock()
	if wrapped == (domain.Address{}) || !due {
		return
	}
	var pools []pool.State
	for _, key := range base.PoolKeys() {
		if p, ok := base.Pool(key); ok {
			pools = append(pools, p)
		}
	}
	paths := conversionPaths(pools, wrapped)
	r.spot.mu.Lock()
	if r.spot.wrapped == wrapped {
		r.spot.paths, r.spot.block = paths, ref.Number
	}
	r.spot.mu.Unlock()
}
