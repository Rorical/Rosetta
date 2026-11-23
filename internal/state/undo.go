package state

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// maxUndoBlocks bounds the committed frames RollbackTo can walk. The open frame
// is kept separately, so a store that has not committed can rewind one block
// further than this bound before the oldest frame is dropped.
const maxUndoBlocks = 8

// undoFrame records the store contents before the first mutation of each pool
// key in the current block. One entry per key is kept, so a rollback restores
// the value from frame start rather than an intermediate one.
type undoFrame struct {
	prevBlock domain.BlockRef
	pools     map[domain.PoolKey]undoEntry
	deepStale map[domain.PoolKey]struct{}
}

// undoEntry is the state of one key at frame start. Presence flags matter: a
// key that did not exist must be deleted on rollback, not restored to a zero
// value.
type undoEntry struct {
	existed bool
	shallow pool.State
	hadDeep bool
	deep    pool.DeepState
	dirty   bool
}

func newUndoFrame(prevBlock domain.BlockRef, stale map[domain.PoolKey]struct{}) *undoFrame {
	return &undoFrame{
		prevBlock: prevBlock,
		pools:     make(map[domain.PoolKey]undoEntry),
		deepStale: copyKeySet(stale),
	}
}

// recordUndo captures key as it stands before the caller mutates it. It is a
// no-op when no frame is open, which keeps Upsert and ApplyV3 usable outside a
// block. Values are copied so later writes to the store cannot change them.
func (s *Store) recordUndo(key domain.PoolKey) {
	if s.undo == nil {
		return
	}
	if _, recorded := s.undo.pools[key]; recorded {
		return
	}

	entry := undoEntry{}
	if prev, ok := s.pools[key]; ok {
		entry.existed = true
		entry.shallow = prev.Copy()
	}
	if prevDeep, ok := s.deep[key]; ok {
		entry.hadDeep = true
		entry.deep = prevDeep.Copy()
	}
	if _, ok := s.dirty[key]; ok {
		entry.dirty = true
	}
	s.undo.pools[key] = entry
}

// Commit accepts the changes made since BeginBlock by moving the open frame
// into the bounded undo history. A later Rollback reports ErrNoUndoFrame
// because no frame is open, but RollbackTo can still undo the committed block
// newest first until its frame is trimmed out of the history.
//
// Keeping the committed frame is what a reorg needs: the block stays undoable
// after the pipeline commits it, so rewinding to a common ancestor restores
// both the head and every value the block's writes replaced.
func (s *Store) Commit() {
	s.commitOpenFrame()
}

// Rollback restores the store to the state it had when BeginBlock opened the
// current frame: the previous block ref, every recorded pool and deep value,
// and the dirty membership of each recorded key. It returns
// ErrNoUndoFrame when no frame is open, which includes the state after Commit.
func (s *Store) Rollback() error {
	frame := s.undo
	if frame == nil {
		return ErrNoUndoFrame
	}
	s.restoreFrame(frame)
	s.undo = nil
	s.invalidateDeepAfter(s.block.Number)
	return nil
}

// RollbackTo rewinds the store until the head number is at or below number. It
// undoes the open frame first and then committed frames from newest to oldest.
// A head already at or below number is left unchanged.
//
// When the oldest retained frame starts above number, the target cannot be
// reached; ErrNoUndoFrame is returned and the store is not modified.
func (s *Store) RollbackTo(number uint64) error {
	if s.block.Number <= number {
		return nil
	}
	oldest, ok := s.oldestUndoBlock()
	if !ok {
		return fmt.Errorf("%w: block %d has no undo frame", ErrNoUndoFrame, s.block.Number)
	}
	if oldest.Number > number {
		return fmt.Errorf("%w: cannot reach block %d, oldest rollback point is %d",
			ErrNoUndoFrame, number, oldest.Number)
	}

	for s.block.Number > number {
		if s.undo != nil {
			s.restoreFrame(s.undo)
			s.undo = nil
			continue
		}
		last := len(s.history) - 1
		s.restoreFrame(s.history[last])
		s.history = s.history[:last]
	}
	s.invalidateDeepAfter(s.block.Number)
	return nil
}

// invalidateDeepAfter drops deep state whose load block cannot be trusted
// after the rollback: anything loaded above the restored head, and anything
// with a zero load block. Deep refreshes run after a block commits, so their
// writes are not in that block's undo frame; without this a pool untouched by
// the replayed branch would keep quoting ticks from the orphan branch. A zero
// LoadedBlock (an old bootstrap without a stamped block, or a restored
// pre-fix snapshot) has no block to compare against and is dropped as unknown
// rather than kept as if it were canonical.
func (s *Store) invalidateDeepAfter(number uint64) {
	for key, deep := range s.deep {
		if deep.LoadedBlock.Number == 0 || deep.LoadedBlock.Number > number {
			delete(s.deep, key)
		}
	}
}

// UndoRefs returns the block refs the store still holds, newest first: the
// current head, the previous block of the open frame when one exists, then the
// previous block of every committed frame from newest to oldest. Recovery uses
// it as the candidate set for a common ancestor: every ref here can be
// restored by RollbackTo.
//
// Zero hashes are placeholders for a store with no earlier block and are
// skipped, as are repeated hashes.
func (s *Store) UndoRefs() []domain.BlockRef {
	refs := make([]domain.BlockRef, 0, len(s.history)+2)
	seen := make(map[domain.Hash]struct{}, len(s.history)+2)
	appendRef := func(ref domain.BlockRef) {
		if ref.Hash == (domain.Hash{}) {
			return
		}
		if _, ok := seen[ref.Hash]; ok {
			return
		}
		seen[ref.Hash] = struct{}{}
		refs = append(refs, ref)
	}

	appendRef(s.block)
	if s.undo != nil {
		appendRef(s.undo.prevBlock)
	}
	for i := len(s.history) - 1; i >= 0; i-- {
		appendRef(s.history[i].prevBlock)
	}
	return refs
}

// oldestUndoBlock returns the lowest block the retained frames can restore.
func (s *Store) oldestUndoBlock() (domain.BlockRef, bool) {
	if len(s.history) > 0 {
		return s.history[0].prevBlock, true
	}
	if s.undo != nil {
		return s.undo.prevBlock, true
	}
	return domain.BlockRef{}, false
}

// commitOpenFrame pushes the completed frame into the bounded history. It is
// called when the next block begins.
func (s *Store) commitOpenFrame() {
	if s.undo == nil {
		return
	}
	s.history = append(s.history, s.undo)
	s.undo = nil
	if len(s.history) > maxUndoBlocks {
		s.history = s.history[len(s.history)-maxUndoBlocks:]
	}
}

// restoreFrame rewinds the block ref and every key the frame recorded.
func (s *Store) restoreFrame(frame *undoFrame) {
	s.block = frame.prevBlock
	// A transaction frame can share the same block number as its predecessor.
	// Keep invalidations from earlier transactions while dropping this frame's.
	s.deepStale = copyKeySet(frame.deepStale)
	s.ensureMaps()
	for key, entry := range frame.pools {
		if entry.existed {
			s.pools[key] = entry.shallow.Copy()
		} else {
			delete(s.pools, key)
		}
		if entry.hadDeep {
			s.deep[key] = entry.deep.Copy()
		} else {
			delete(s.deep, key)
		}
		if entry.dirty {
			s.dirty[key] = struct{}{}
		} else {
			delete(s.dirty, key)
		}
	}
}
