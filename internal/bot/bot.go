// Package bot binds one instance to a backend supplied by the executable.
package bot

import (
	"context"
	"fmt"
	"github.com/Rorical/Rosetta/internal/chain"
	"github.com/Rorical/Rosetta/internal/domain"
	"io"

	"github.com/Rorical/Rosetta/internal/config"
)

// Run parses the command options and follows feeds until ctx is canceled.
func Run(ctx context.Context, args []string, output io.Writer, factory chain.Factory) error {
	cfg, err := config.Parse(args, output)
	if err != nil {
		return err
	}
	return RunConfigured(ctx, cfg, factory)
}

// RunConfigured validates backend identity before it can start consuming state.
func RunConfigured(ctx context.Context, cfg config.Config, factory chain.Factory) error {
	resolved, err := cfg.ResolveNetwork()
	if err != nil {
		return err
	}
	if factory == nil {
		return fmt.Errorf("execution backend factory is required")
	}
	backend, err := factory.Create(ctx, resolved)
	if err != nil {
		return err
	}
	expected := domain.ChainIdentity{Family: resolved.Network.Family, ChainID: resolved.ChainID()}
	if backend == nil || backend.Identity() != expected {
		return fmt.Errorf("execution backend identity differs from selected network")
	}
	return backend.Run(ctx)
}
