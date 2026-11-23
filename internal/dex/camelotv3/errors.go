package camelotv3

import "errors"

// Sentinel errors for loader and adapter failures.
var (
	ErrInvalidSpec     = errors.New("camelotv3: invalid spec")
	ErrInvalidConfig   = errors.New("camelotv3: invalid config")
	ErrInvalidResponse = errors.New("camelotv3: invalid call response")
	ErrTokenNotInPool  = errors.New("camelotv3: token not in pool")
	ErrInvalidLog      = errors.New("camelotv3: invalid metadata log")
)
