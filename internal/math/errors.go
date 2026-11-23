package math

import "errors"

// Sentinel errors. Functions wrap them with context; use errors.Is to test.
var (
	ErrInvalidArgument     = errors.New("math: invalid argument")
	ErrTickOutOfRange      = errors.New("math: tick out of range")
	ErrSqrtPriceOutOfRange = errors.New("math: sqrt price out of range")
	ErrInvalidState        = errors.New("math: invalid state")
	ErrMissingTick         = errors.New("math: initialized tick data missing")
	ErrOutOfWindow         = errors.New("math: swap leaves the loaded tick window")
	ErrCapacity            = errors.New("math: swap exceeds the modeled liquidity")
)
