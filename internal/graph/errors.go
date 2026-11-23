package graph

import "errors"

// Sentinel errors for graph construction and table limits.
var (
	ErrInvalidPool      = errors.New("graph: pool has identical tokens")
	ErrDuplicatePool    = errors.New("graph: duplicate pool address")
	ErrNilGraph         = errors.New("graph: nil graph")
	ErrInvalidDepth     = errors.New("graph: max depth out of range")
	ErrInvalidMaxCycles = errors.New("graph: max cycles must be positive")
	ErrInvalidPoolTopK  = errors.New("graph: pool top k must be positive")
	ErrInvalidCycle     = errors.New("graph: invalid cycle")
	ErrNilCycleSource   = errors.New("graph: nil cycle source")
	ErrTableFull        = errors.New("cycle table full")
)
