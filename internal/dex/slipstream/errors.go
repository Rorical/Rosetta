package slipstream

import "errors"

// ErrInvalidConfig reports fee or tick spacing outside the V3-shaped bounds.
var ErrInvalidConfig = errors.New("slipstream: invalid config")

// ErrInvalidResponse reports a fee() call that returned malformed or
// out-of-range data.
var ErrInvalidResponse = errors.New("slipstream: invalid call response")

// ErrInvalidSpec reports unusable deep or fee loader input: a nil caller, a
// zero address, a non-Slipstream kind, or an empty block.
var ErrInvalidSpec = errors.New("slipstream: invalid spec")
