package arb

import "errors"

// Sentinel errors for engine configuration and evaluation.
var (
	ErrInvalidConfig = errors.New("arb: invalid config")
	ErrNilView       = errors.New("arb: nil view")
)
