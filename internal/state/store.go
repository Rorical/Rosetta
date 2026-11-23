package state

import (
	"fmt"
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/dex/slipstream"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Store is the in-memory chain state: the current block plus the shallow and
// deep state of known pools. Mutating methods copy on write so callers never
// share memory with the store. A Store is not safe for concurrent use.
type Store struct {
	block     domain.BlockRef
	pools     map[domain.PoolKey]pool.State
	deep      map[domain.PoolKey]pool.DeepState
	dirty     map[domain.PoolKey]struct{}
	deepStale map[domain.PoolKey]struct{}
	undo      *undoFrame   // nil when no block is open for rollback
	history   []*undoFrame // completed blocks, oldest first, bounded by maxUndoBlocks
}

// New returns an empty store at the zero block.
func New() *Store {
	return &Store{
		pools:     make(map[domain.PoolKey]pool.State),
		deep:      make(map[domain.PoolKey]pool.DeepState),
		dirty:     make(map[domain.PoolKey]struct{}),
		deepStale: make(map[domain.PoolKey]struct{}),
	}
}

// BeginBlock advances the chain head to ref and opens a new undo frame. A
// frame left open by the previous block is first moved into the bounded undo
// history, so each BeginBlock implicitly commits the block before it. A block
// number equal to the current head is accepted; a lower number is rejected
// because rolling back requires restoring a snapshot first.
func (s *Store) BeginBlock(ref domain.BlockRef) error {
	if ref.Number < s.block.Number {
		return fmt.Errorf("%w: block %d below current %d", ErrBlockOutOfOrder, ref.Number, s.block.Number)
	}
	s.commitOpenFrame()
	s.undo = newUndoFrame(s.block, s.deepStale)
	s.block = ref
	s.advanceSlipstream(ref)
	return nil
}

// Block returns the current chain head.
func (s *Store) Block() domain.BlockRef {
	return s.block
}

// Upsert stores a copy of p keyed by p.Key and marks the pool dirty. When the
// identity of p differs from the stored state under the same key, the deep
// state belongs to a different pool and is dropped before the shallow state is
// replaced. An unchanged identity is a shallow refresh and keeps the deep
// state. The key's previous contents are recorded in the open undo frame, if
// any.
func (s *Store) Upsert(p pool.State) error {
	key := p.Key
	if key.Address == (domain.Address{}) {
		return fmt.Errorf("%w: zero pool address", pool.ErrInvalidState)
	}
	if p.FeeOneForZero != nil && (key.Kind != domain.PoolKindCamelotV3 || *p.FeeOneForZero >= 1_000_000) {
		return fmt.Errorf("%w: directional fee requires a Camelot pool and a value below 1000000", pool.ErrInvalidState)
	}
	if err := slipstream.ValidateState(p.Slipstream); err != nil {
		return err
	}
	s.ensureMaps()
	s.recordUndo(key)
	if existing, ok := s.pools[key]; ok && !samePoolIdentity(existing, p) {
		delete(s.deep, key)
	}
	s.dropOtherKinds(key)
	s.pools[key] = p.Copy()
	s.dirty[key] = struct{}{}
	// A pool without stored ticks has nothing a quote can use. Marking it stale
	// also covers a shallow replacement that dropped the old pool's deep state;
	// a same-identity refresh such as the fee reload keeps the ticks and stays
	// unmarked.
	if isV3Kind(key.Kind) {
		if _, ok := s.deep[key]; !ok {
			s.deepStale[key] = struct{}{}
		}
	}
	return nil
}

// dropOtherKinds removes the pools that share key's address but belong to a
// different family, including their deep state and dirty marker. An address
// names one pool per family, so a shallow replacement that changes kind would
// otherwise leave the old family stale, and a leftover dirty marker would name
// a pool that no longer exists. V4 pools share both the
// PoolManager address and the kind, so siblings behind one manager are never
// dropped.
func (s *Store) dropOtherKinds(key domain.PoolKey) {
	for _, kind := range [...]domain.PoolKind{
		domain.PoolKindV2,
		domain.PoolKindV3,
		domain.PoolKindV4,
		domain.PoolKindCamelotV3,
		domain.PoolKindSlipstream,
		domain.PoolKindVelodromeV2,
		domain.PoolKindSolidlyV3,
	} {
		if kind == key.Kind {
			continue
		}
		other := domain.PoolKey{Address: key.Address, Kind: kind}
		if _, ok := s.pools[other]; !ok {
			continue
		}
		s.recordUndo(other)
		delete(s.pools, other)
		delete(s.deep, other)
		delete(s.dirty, other)
		delete(s.deepStale, other)
	}
}

// Remove deletes the pool stored under key with its deep state and markers.
// Its previous contents are recorded in the open undo frame, if any.
func (s *Store) Remove(key domain.PoolKey) {
	if _, ok := s.pools[key]; !ok {
		return
	}
	s.recordUndo(key)
	delete(s.pools, key)
	delete(s.deep, key)
	delete(s.dirty, key)
	delete(s.deepStale, key)
}

// Pool returns a copy of the shallow state for key. Mutating the result does
// not affect the store.
func (s *Store) Pool(key domain.PoolKey) (pool.State, bool) {
	p, ok := s.pools[key]
	if !ok {
		return pool.State{}, false
	}
	return p.Copy(), true
}

// Len returns the tracked pool count without copying state.
func (s *Store) Len() int { return len(s.pools) }

// Pools returns a copy of every pool's shallow state, ordered by PoolKey.
// Big integers are cloned, so mutating the result does not affect the store.
// Deep state is not included.
func (s *Store) Pools() []pool.State {
	pools := make([]pool.State, 0, len(s.pools))
	for _, p := range s.pools {
		pools = append(pools, p.Copy())
	}
	slices.SortFunc(pools, func(a, b pool.State) int {
		return domain.ComparePoolKeys(a.Key, b.Key)
	})
	return pools
}

// Deep returns a copy of the tick-level state for key. Mutating the result does
// not affect the store.
func (s *Store) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	deep, ok := s.deep[key]
	if !ok {
		return pool.DeepState{}, false
	}
	return deep.Copy(), true
}

// DeepStatus reports the freshness fields of key's deep state without copying
// its bitmap or ticks. A per-block refresh uses it to decide whether Deep is
// worth the copy.
func (s *Store) DeepStatus(key domain.PoolKey) (windowMin, windowMax int32, loaded domain.BlockRef, ok bool) {
	deep, ok := s.deep[key]
	if !ok {
		return 0, 0, domain.BlockRef{}, false
	}
	return deep.WindowMin, deep.WindowMax, deep.LoadedBlock, true
}

// SetDeep replaces the tick-level state for key. It returns ErrPoolNotFound
// when no shallow state exists, because deep state without a pool would fail
// snapshot validation and cannot be interpreted. The caller's maps are copied
// and nil maps are stored as empty maps, so a loaded-but-empty pool is
// distinguishable from one that was never loaded. Deep state does not change
// the shallow graph, so the pool is not marked dirty. The previous deep value
// is recorded in the open undo frame, if any.
func (s *Store) SetDeep(key domain.PoolKey, deep pool.DeepState) error {
	if _, ok := s.pools[key]; !ok {
		return fmt.Errorf("%w: %s", ErrPoolNotFound, formatPoolKey(key))
	}

	stored := normalizeDeep(deep)

	s.ensureMaps()
	s.recordUndo(key)
	s.deep[key] = stored
	delete(s.deepStale, key)
	return nil
}

// ApplyV3 applies ev to the pool at key and stamps the result with
// blockNumber. The write is transactional: when uniswapv3.Apply fails, the
// store keeps the pre-event state and dirty set. A pool whose key is not a
// V3-shaped kind (V3, Camelot V3, Slipstream, V4) is rejected before the event
// is applied. A successful event records key's previous contents in the open
// undo frame, if any.
func (s *Store) ApplyV3(key domain.PoolKey, ev uniswapv3.Event, blockNumber uint64) error {
	current, ok := s.pools[key]
	if !ok {
		return fmt.Errorf("%w: %s", ErrPoolNotFound, formatPoolKey(key))
	}
	if !isV3Kind(current.Key.Kind) {
		return fmt.Errorf("%w: pool %s kind %d", ErrInvalidPoolKind, formatPoolKey(key), current.Key.Kind)
	}

	state := current.Copy()
	if state.Slipstream != nil {
		slipstream.FollowEvent(&state, ev)
	}
	preservesWindow := preservesTickWindow(s.deep[key], ev)
	deep := s.deep[key].Copy()
	if err := uniswapv3.Apply(&state, &deep, ev); err != nil {
		return fmt.Errorf("state: apply v3 event for pool %s: %w", formatPoolKey(key), err)
	}
	if state.Slipstream != nil {
		slipstream.RefreshFee(&state)
	}
	state.UpdatedBlock = blockNumber

	s.ensureMaps()
	s.recordUndo(key)
	s.pools[key] = state
	s.deep[key] = deep
	s.dirty[key] = struct{}{}
	if eventTouchesDeep(ev) && !preservesWindow {
		s.deepStale[key] = struct{}{}
	}
	return nil
}

// eventTouchesDeep reports whether an event can change initialized-tick
// liquidity. A swap only moves the price and active liquidity, so the bitmap
// and tick net liquidity stay valid. A zero-amount mint or burn is a chain
// no-op and changes no tick, so it must not force a deep refresh.
func eventTouchesDeep(ev uniswapv3.Event) bool {
	switch ev.Kind {
	case uniswapv3.EventInitialize:
		return true
	case uniswapv3.EventMint, uniswapv3.EventBurn:
		return ev.Amount != nil && ev.Amount.Sign() > 0
	default:
		return false
	}
}

// Dirty returns the keys changed since the last ClearDirty, ordered by
// PoolKey.
func (s *Store) Dirty() []domain.PoolKey {
	dirty := make([]domain.PoolKey, 0, len(s.dirty))
	for key := range s.dirty {
		dirty = append(dirty, key)
	}
	slices.SortFunc(dirty, domain.ComparePoolKeys)
	return dirty
}

// ClearDirty drops the dirty set without touching pool state.
func (s *Store) ClearDirty() {
	clear(s.dirty)
}

// DeepStale returns the pools whose ticks a block event invalidated, ordered by
// PoolKey. A marker says the stored deep state may still cover the loaded
// window but its tick contents are no longer trustworthy, so the next refresh
// must reload it. SetDeep clears the marker; a swap does not set one.
func (s *Store) DeepStale() []domain.PoolKey {
	stale := make([]domain.PoolKey, 0, len(s.deepStale))
	for key := range s.deepStale {
		stale = append(stale, key)
	}
	slices.SortFunc(stale, domain.ComparePoolKeys)
	return stale
}

// formatPoolKey renders a key for an error message. V4 pools share the
// PoolManager address, so the pool ID is part of the name when it is set.
func formatPoolKey(key domain.PoolKey) string {
	if key.PoolID == (domain.Hash{}) {
		return key.Address.Hex()
	}
	return key.Address.Hex() + "/" + key.PoolID.Hex()
}

// normalizeDeep returns an independent copy of deep with non-nil maps, so a
// loaded-but-empty pool is distinguishable from one that was never loaded.
func normalizeDeep(deep pool.DeepState) pool.DeepState {
	stored := deep.Copy()
	if stored.Bitmap == nil {
		stored.Bitmap = make(map[int16]*big.Int)
	}
	if stored.Ticks == nil {
		stored.Ticks = make(map[int32]pool.TickInfo)
	}
	return stored
}

// samePoolIdentity reports whether a and b describe the same pool under one
// key. Deep state is keyed by tick, so a different kind, token pair, or tick
// spacing makes the stored ticks meaningless. Fee only affects pricing, not
// tick layout, so a fee change does not invalidate deep state.
func samePoolIdentity(a, b pool.State) bool {
	return a.Key.Kind == b.Key.Kind &&
		a.Token0 == b.Token0 &&
		a.Token1 == b.Token1 &&
		a.TickSpacing == b.TickSpacing
}

// isV3Kind reports whether kind uses the V3 state shape: sqrtPriceX96, tick,
// liquidity, and a tick bitmap. Camelot V3, Slipstream, and V4 reuse that shape
// even though their loaders, fee rules, and pricing adapters differ.
func isV3Kind(kind domain.PoolKind) bool {
	switch kind {
	case domain.PoolKindV3, domain.PoolKindCamelotV3, domain.PoolKindSlipstream, domain.PoolKindV4, domain.PoolKindSolidlyV3:
		return true
	default:
		return false
	}
}

// ensureMaps lets a zero Store accept writes; New remains the preferred
// constructor.
func (s *Store) ensureMaps() {
	if s.pools == nil {
		s.pools = make(map[domain.PoolKey]pool.State)
	}
	if s.deep == nil {
		s.deep = make(map[domain.PoolKey]pool.DeepState)
	}
	if s.dirty == nil {
		s.dirty = make(map[domain.PoolKey]struct{})
	}
	if s.deepStale == nil {
		s.deepStale = make(map[domain.PoolKey]struct{})
	}
}
