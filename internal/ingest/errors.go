package ingest

import "errors"

// Sentinel errors for routing setup and block validation failures.
var (
	ErrInvalidConfig = errors.New("ingest: invalid config")
	// ErrReorg reports a block that does not extend the accepted head.
	ErrReorg = errors.New("ingest: reorg detected")
)
