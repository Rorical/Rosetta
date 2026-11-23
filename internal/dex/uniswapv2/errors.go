package uniswapv2

import "errors"

// Sentinel errors for adapter failures.
var (
	ErrInvalidConfig   = errors.New("uniswapv2: invalid config")
	ErrTokenNotInPool  = errors.New("uniswapv2: token not in pool")
	ErrInvalidLog      = errors.New("uniswapv2: invalid log")
	ErrInvalidSpec     = errors.New("uniswapv2: invalid spec")
	ErrInvalidResponse = errors.New("uniswapv2: invalid call response")
)
