package ingest

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
)

// routing separates the two log shapes: non-V4 pools are addressed by their
// own contract, V4 pools by the pool ID in the log topic behind the shared
// PoolManager address.
type routing struct {
	byAddress  map[domain.Address]domain.PoolKey
	byPoolID   map[domain.Hash]domain.PoolKey
	v4Managers map[domain.Address]struct{}
}

func newRouting(keys []domain.PoolKey) (routing, error) {
	r := routing{
		byAddress:  make(map[domain.Address]domain.PoolKey, len(keys)),
		byPoolID:   make(map[domain.Hash]domain.PoolKey),
		v4Managers: make(map[domain.Address]struct{}),
	}
	for _, key := range keys {
		if key.Address == (domain.Address{}) {
			return routing{}, fmt.Errorf("%w: pool key with zero address", ErrInvalidConfig)
		}
		if key.Kind == domain.PoolKindWrap {
			// The wrapped native token has no pool state to follow, and its
			// address emits a log for every transfer of the token.
			continue
		}
		if key.IsV4() {
			if key.PoolID == (domain.Hash{}) {
				return routing{}, fmt.Errorf("%w: v4 pool %s has zero pool id", ErrInvalidConfig, key.Address)
			}
			if _, exists := r.byAddress[key.Address]; exists {
				return routing{}, fmt.Errorf("%w: v4 pool %s shares an address with a non-v4 pool",
					ErrInvalidConfig, key.Address)
			}
			if _, exists := r.byPoolID[key.PoolID]; exists {
				return routing{}, fmt.Errorf("%w: duplicate v4 pool id %s", ErrInvalidConfig, key.PoolID.Hex())
			}
			r.byPoolID[key.PoolID] = key
			r.v4Managers[key.Address] = struct{}{}
			continue
		}
		if _, exists := r.v4Managers[key.Address]; exists {
			return routing{}, fmt.Errorf("%w: pool %s shares an address with a v4 pool",
				ErrInvalidConfig, key.Address)
		}
		if _, exists := r.byAddress[key.Address]; exists {
			return routing{}, fmt.Errorf("%w: duplicate pool address %s", ErrInvalidConfig, key.Address)
		}
		r.byAddress[key.Address] = key
	}
	return r, nil
}

// resolveKey maps one log to its pool. A log at a tracked pool contract wins;
// otherwise a log at a tracked PoolManager resolves through its pool ID topic.
// Logs that match neither are not ours.
func (r routing) resolveKey(log Log) (domain.PoolKey, bool) {
	if key, ok := r.byAddress[log.Address]; ok {
		return key, true
	}
	if _, ok := r.v4Managers[log.Address]; !ok || len(log.Topics) < 2 {
		return domain.PoolKey{}, false
	}
	key, ok := r.byPoolID[log.Topics[1]]
	return key, ok && key.Address == log.Address
}
