package state

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// View is an immutable pool snapshot for search workers. Store mutations copy
// values before replacement, so capture only clones the top-level maps. Access
// methods copy values back out, including tick maps and big integers.
type View struct {
	block domain.BlockRef
	pools map[domain.PoolKey]pool.State
	deep  map[domain.PoolKey]pool.DeepState
	stale map[domain.PoolKey]struct{}
}

// View must be captured on the store's writer goroutine. The returned value can
// then be read concurrently with further store writes or rollbacks.
func (s *Store) View() View {
	return View{block: s.block, pools: maps.Clone(s.pools), deep: maps.Clone(s.deep), stale: maps.Clone(s.deepStale)}
}

func (v View) Block() domain.BlockRef { return v.block }

// PoolKeys lists this snapshot's pool identities in deterministic order.
func (v View) PoolKeys() []domain.PoolKey {
	keys := make([]domain.PoolKey, 0, len(v.pools))
	for key := range v.pools {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return keys
}

// Fork creates an independent writer over an immutable view, without undo
// history. Stored values remain shared until a copy-on-write replacement.
func (v View) Fork() *Store {
	return &Store{block: v.block, pools: maps.Clone(v.pools), deep: maps.Clone(v.deep), deepStale: maps.Clone(v.stale), dirty: make(map[domain.PoolKey]struct{})}
}

// Fork copies the writer's maps and markers without undo history. Like View,
// it shares stored values until a write replaces them. Capture on the source
// writer goroutine; the two stores can then be driven independently.
func (s *Store) Fork() *Store {
	return &Store{block: s.block, pools: maps.Clone(s.pools), deep: maps.Clone(s.deep), deepStale: maps.Clone(s.deepStale), dirty: maps.Clone(s.dirty)}
}

func (v View) Pool(key domain.PoolKey) (pool.State, bool) {
	p, ok := v.pools[key]
	return p.Copy(), ok
}

// Deep refuses ticks marked stale; search can request a refresh rather than
// quote an out-of-date liquidity window.
func (v View) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	if _, stale := v.stale[key]; stale {
		return pool.DeepState{}, false
	}
	d, ok := v.deep[key]
	return d.Copy(), ok
}

// SharedDeep is Deep without the copy, for readers that only quote. The store
// replaces a pool's tick state instead of changing it, so the returned maps
// and integers never change; the caller must not change them either.
func (v View) SharedDeep(key domain.PoolKey) (pool.DeepState, bool) {
	if _, stale := v.stale[key]; stale {
		return pool.DeepState{}, false
	}
	d, ok := v.deep[key]
	return d, ok
}

// EncodeSnapshotAt encodes an immutable standard working view with final
// verified header metadata. It cannot turn an unrelated or Fast view into a
// canonical snapshot. Encoding can run on a persistence worker.
func (v View) EncodeSnapshotAt(ref domain.BlockRef) ([]byte, error) {
	if ref.Hash == (domain.Hash{}) || ref.Number != v.block.Number || ref.Hash != v.block.Hash || ref.ParentHash != v.block.ParentHash {
		return nil, fmt.Errorf("%w: snapshot header identity differs from view", ErrInvalidSnapshot)
	}
	return json.Marshal(snapshotToWire(Snapshot{block: ref, pools: v.pools, deep: v.deep, deepStale: v.stale}))
}
