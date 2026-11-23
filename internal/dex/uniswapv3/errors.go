package uniswapv3

import "errors"

// Sentinel errors for adapter and event-reducer failures.
var (
	ErrInvalidConfig         = errors.New("uniswapv3: invalid config")
	ErrTokenNotInPool        = errors.New("uniswapv3: token not in pool")
	ErrInvalidEvent          = errors.New("uniswapv3: invalid event")
	ErrInsufficientLiquidity = errors.New("uniswapv3: insufficient liquidity")
	ErrInvalidLog            = errors.New("uniswapv3: invalid log")
	ErrInvalidSpec           = errors.New("uniswapv3: invalid spec")
	ErrInvalidResponse       = errors.New("uniswapv3: invalid call response")
	// ErrUnsupportedKind reports a pool kind whose tick state is not stored in
	// the V3 bitmap layout.
	ErrUnsupportedKind = errors.New("uniswapv3: unsupported pool kind")
)
