package arb

import (
	"context"
	"hash/maphash"
	"math/big"
	"sync"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

type legQuoteKey struct {
	pool   domain.PoolKey
	token  domain.Address
	amount [32]byte
}
type legQuoteShard struct {
	sync.Mutex
	values map[legQuoteKey]*big.Int
	order  []legQuoteKey
	next   int
}

// A memo belongs to one prepared job, never to a chain root across jobs. Each
// pool is prepared once; cached successful quotes therefore have the same state
// and tick window. Errors are retriable and are never retained. Concurrent misses
// compute independently rather than holding a shard lock during protocol math.
type legQuoteMemo struct {
	seed   maphash.Seed
	shards [16]legQuoteShard
}

func newLegQuoteMemo() *legQuoteMemo { return &legQuoteMemo{seed: maphash.MakeSeed()} }

func (m *legQuoteMemo) quote(ctx context.Context, q Quoter, state pool.State, deep pool.DeepState, token domain.Address, amount *big.Int) (*big.Int, error) {
	if amount.BitLen() > 256 {
		return q.QuoteExactIn(ctx, state, deep, token, amount)
	}
	key := legQuoteKey{state.Key, token, amountKey(amount)}
	shard := &m.shards[maphash.Comparable(m.seed, key)%uint64(len(m.shards))]
	shard.Lock()
	value := shard.values[key]
	shard.Unlock()
	if value != nil {
		return new(big.Int).Set(value), nil
	}
	out, err := q.QuoteExactIn(ctx, state, deep, token, amount)
	if err != nil {
		return nil, err
	}
	owned := new(big.Int).Set(out)
	shard.Lock()
	if shard.values == nil {
		shard.values = make(map[legQuoteKey]*big.Int)
	}
	if _, exists := shard.values[key]; !exists {
		if len(shard.order) == 256 {
			delete(shard.values, shard.order[shard.next])
			shard.order[shard.next] = key
			shard.next = (shard.next + 1) % len(shard.order)
		} else {
			shard.order = append(shard.order, key)
		}
		shard.values[key] = owned
	}
	shard.Unlock()
	return out, nil
}
