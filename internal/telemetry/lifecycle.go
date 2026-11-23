package telemetry

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"time"
)

// Options selects metric sinks. Collect keeps a registry alive for a caller
// that serves snapshots itself, without opening a TCP port or output file.
type Options struct {
	Listen, Output string
	Collect        bool
	Build          Build
}

// WriteFile writes the current snapshot to a new file, as the -metrics-out
// sink does at shutdown. Rotated runs use it to close each run's metrics.
func (r *Registry) WriteFile(path string) error {
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("create metrics output: %w", err)
	}
	return errors.Join(json.NewEncoder(output).Encode(r.Snapshot()), output.Close())
}

// Start before workers, close after they join. This makes finite-run snapshots
// include canceled work and checkpoint jobs drained during shutdown.
func Start(options Options) (*Registry, func() error, error) {
	listen, path := options.Listen, options.Output
	if listen == "" && path == "" && !options.Collect {
		return nil, func() error { return nil }, nil
	}
	registry := &Registry{build: options.Build, started: time.Now()}
	var output *os.File
	if path != "" {
		var err error
		output, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, nil, fmt.Errorf("create metrics output: %w", err)
		}
	}
	var server *http.Server
	var done chan error
	if listen != "" {
		listener, err := net.Listen("tcp", listen)
		if err != nil {
			if output != nil {
				output.Close()
			}
			return nil, nil, fmt.Errorf("listen for metrics: %w", err)
		}
		server = &http.Server{Handler: registry, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
		done = make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
	}
	return registry, func() error {
		var result error
		if server != nil {
			result = server.Close()
			if err := <-done; !errors.Is(err, http.ErrServerClosed) {
				result = errors.Join(result, err)
			}
		}
		if output != nil {
			result = errors.Join(result, json.NewEncoder(output).Encode(registry.Snapshot()), output.Close())
		}
		return result
	}, nil
}
