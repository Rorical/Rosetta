package pool

import "errors"

// Sentinel errors for adapter and state failures.
var (
	ErrNoLiquidity  = errors.New("pool: no liquidity")
	ErrInvalidState = errors.New("pool: invalid state")
	// ErrCallRejected is a contract execution failure, separate from missing
	// state or transport errors. Discovery can skip that pool and keep scanning.
	ErrCallRejected = errors.New("pool: contract read rejected")
)
