package uniswapv4

import "errors"

// ErrInvalidLog reports a PoolManager log with a missing pool ID or malformed
// data.
var ErrInvalidLog = errors.New("uniswapv4: invalid log")

// ErrInvalidSpec reports unusable loader input: a nil caller, zero pool ID, or
// empty block.
var ErrInvalidSpec = errors.New("uniswapv4: invalid spec")

// ErrInvalidResponse reports a StateView call that returned malformed or
// out-of-range data.
var ErrInvalidResponse = errors.New("uniswapv4: invalid call response")

// ErrUnsupportedKind reports a pool kind whose tick state is not stored in the
// V3 bitmap layout.
var ErrUnsupportedKind = errors.New("uniswapv4: unsupported pool kind")

// ErrUnsupportedHooks reports a V4 pool whose hooks can change swap behavior.
var ErrUnsupportedHooks = errors.New("uniswapv4: hooks not supported")

// ErrInvalidFee reports a slot0 fee that cannot be combined into a swap fee.
var ErrInvalidFee = errors.New("uniswapv4: invalid fee")

// ErrMissingTickState reports a quote without the bitmap and tick maps the
// swap needs to cross initialized ticks.
var ErrMissingTickState = errors.New("uniswapv4: missing tick state")

// ErrTokenNotInPool reports a quote for a token that is neither pool token.
var ErrTokenNotInPool = errors.New("uniswapv4: token not in pool")
