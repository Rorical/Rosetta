package state

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Snapshot is a point-in-time copy of a store. Its contents stay unexported so
// callers cannot edit a captured snapshot; Restore copies it back into a store.
type Snapshot struct {
	block     domain.BlockRef
	pools     map[domain.PoolKey]pool.State
	deep      map[domain.PoolKey]pool.DeepState
	dirty     map[domain.PoolKey]struct{}
	deepStale map[domain.PoolKey]struct{}
}

// DeepDelta is one pool's deep state paired with its key, because
// pool.DeepState does not carry one.
type DeepDelta struct {
	Key   domain.PoolKey
	State pool.DeepState
}

// Snapshot returns an independent copy of the store and makes it the rollback
// checkpoint: any open undo frame is closed and the undo history is cleared.
// Persistence does not use this method; EncodeSnapshot copies through
// Checkpoint so periodic writes keep the reorg window.
func (s *Store) Snapshot() Snapshot {
	checkpoint := s.Checkpoint()
	s.undo = nil
	s.history = nil
	return checkpoint
}

// Checkpoint returns an independent copy of the store without touching the open
// undo frame or the history. It is the capture to use for a failure rollback
// that must keep the reorg window intact.
func (s *Store) Checkpoint() Snapshot {
	return Snapshot{
		block:     s.block,
		pools:     copyPools(s.pools),
		deep:      copyDeepStates(s.deep),
		dirty:     copyKeySet(s.dirty),
		deepStale: copyKeySet(s.deepStale),
	}
}

// Restore replaces the store contents with an independent copy of snap and
// clears any open undo frame and the undo history; a restored store has nothing
// to roll back. Stale markers travel with the snapshot: a persisted marker must
// survive a restart so the first deep refresh reloads the pool instead of
// trusting tick state the snapshot could not prove current. An invalid snapshot
// leaves the store unchanged, including an open frame.
func (s *Store) Restore(snap Snapshot) error {
	if err := snap.validate(); err != nil {
		return err
	}
	s.block = snap.block
	s.pools = copyPools(snap.pools)
	s.deep = copyDeepStates(snap.deep)
	s.dirty = copyKeySet(snap.dirty)
	s.deepStale = copyKeySet(snap.deepStale)
	s.undo = nil
	s.history = nil
	return nil
}

func (snap Snapshot) validate() error {
	if snap.pools == nil || snap.deep == nil || snap.dirty == nil || snap.deepStale == nil {
		return fmt.Errorf("%w: nil maps", ErrInvalidSnapshot)
	}
	kinds := make(map[domain.Address]domain.PoolKind, len(snap.pools))
	for key, p := range snap.pools {
		if key.Address == (domain.Address{}) {
			return fmt.Errorf("%w: zero pool address", ErrInvalidSnapshot)
		}
		if p.Key != key {
			return fmt.Errorf("%w: pool key %s stored under %s",
				ErrInvalidSnapshot, formatPoolKey(p.Key), formatPoolKey(key))
		}
		if previous, ok := kinds[key.Address]; ok && previous != key.Kind {
			return fmt.Errorf("%w: kinds %d and %d share address %s",
				ErrInvalidSnapshot, previous, key.Kind, key.Address.Hex())
		}
		kinds[key.Address] = key.Kind
	}
	for key := range snap.deep {
		if _, ok := snap.pools[key]; !ok {
			return fmt.Errorf("%w: deep state for unknown pool %s", ErrInvalidSnapshot, formatPoolKey(key))
		}
	}
	for key := range snap.dirty {
		if _, ok := snap.pools[key]; !ok {
			return fmt.Errorf("%w: dirty marker for unknown pool %s", ErrInvalidSnapshot, formatPoolKey(key))
		}
	}
	for key := range snap.deepStale {
		if _, ok := snap.pools[key]; !ok {
			return fmt.Errorf("%w: stale marker for unknown pool %s", ErrInvalidSnapshot, formatPoolKey(key))
		}
	}
	return nil
}

func copyPools(pools map[domain.PoolKey]pool.State) map[domain.PoolKey]pool.State {
	copied := make(map[domain.PoolKey]pool.State, len(pools))
	for key, p := range pools {
		copied[key] = p.Copy()
	}
	return copied
}

func copyDeepStates(deep map[domain.PoolKey]pool.DeepState) map[domain.PoolKey]pool.DeepState {
	copied := make(map[domain.PoolKey]pool.DeepState, len(deep))
	for key, state := range deep {
		copied[key] = state.Copy()
	}
	return copied
}

func copyKeySet(keys map[domain.PoolKey]struct{}) map[domain.PoolKey]struct{} {
	copied := make(map[domain.PoolKey]struct{}, len(keys))
	for key := range keys {
		copied[key] = struct{}{}
	}
	return copied
}
