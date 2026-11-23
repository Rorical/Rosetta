package simulate

import "errors"

// ErrInvalidConfig reports a Quoter with a missing or unusable dependency.
var ErrInvalidConfig = errors.New("simulate: invalid config")
