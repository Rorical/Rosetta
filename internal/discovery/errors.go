package discovery

import "errors"

var (
	// ErrInvalidConfig reports an unusable Config or configuration document.
	ErrInvalidConfig = errors.New("discovery: invalid config")
	// ErrInvalidLog reports a creation log whose shape does not match its
	// event. Discovery counts it and keeps scanning.
	ErrInvalidLog = errors.New("discovery: invalid log")
)
