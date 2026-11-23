// Package chain defines the process boundary for chain-specific execution
// backends. It does not import any execution client or register global hooks.
package chain

import (
	"context"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/domain"
)

// Runtime owns one chain's state writer, transports and recovery lifecycle.
// Run must join its workers and release state leases before returning.
type Runtime interface {
	Identity() domain.ChainIdentity
	Run(context.Context) error
}

// Factory is supplied by the executable, keeping client forks out of shared
// orchestration. Create returns a dormant runtime without acquiring resources;
// Run owns acquisition and cleanup. Unsupported families must be rejected.
type Factory interface {
	Create(context.Context, config.Config) (Runtime, error)
}
