package state

import "errors"

// Sentinel errors for callers that need to branch on the failure class.
var (
	ErrPoolNotFound    = errors.New("state: pool not found")
	ErrBlockOutOfOrder = errors.New("state: block out of order")
	ErrInvalidSnapshot = errors.New("state: invalid snapshot")
	// ErrInvalidPoolKind reports an event applied to a pool of the wrong
	// protocol family. It is separate from pool.ErrInvalidState, which marks
	// unusable field values, so callers can tell a routing mistake from bad
	// pool data.
	ErrInvalidPoolKind = errors.New("state: invalid pool kind")
	// ErrNoUndoFrame reports a rollback with no block frame left to undo,
	// either because BeginBlock was never called or Commit already accepted
	// the changes.
	ErrNoUndoFrame = errors.New("state: no undo frame")
)
