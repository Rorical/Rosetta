package domain

import "errors"

// Sentinel errors for callers that need to branch on the failure class.
// Functions wrap them with context; use errors.Is to test.
var (
	ErrInvalidAddress = errors.New("domain: invalid address")
	ErrInvalidHash    = errors.New("domain: invalid hash")
	ErrUnsupported    = errors.New("domain: unsupported")
	ErrNotFound       = errors.New("domain: not found")
	ErrStale          = errors.New("domain: stale state")
)
