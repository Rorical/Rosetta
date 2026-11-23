package telemetry

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"time"
)

// StartProfiling serves Go runtime profiles under /debug/pprof/ on a loopback
// address. Profiles reveal stacks and allocation sites, so other interfaces are
// refused, and the command line is not served because it carries endpoint
// credentials. There is no write timeout: a CPU profile streams for as long as
// the request asks.
func StartProfiling(listen string) (func() error, error) {
	if listen == "" {
		return func() error { return nil }, nil
	}
	host, _, err := net.SplitHostPort(listen)
	if err != nil {
		return nil, fmt.Errorf("profile listen address: %w", err)
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("profile listen address %s is not a loopback IP", listen)
	}
	listener, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, fmt.Errorf("listen for profiles: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	return func() error {
		result := server.Close()
		if err := <-done; !errors.Is(err, http.ErrServerClosed) {
			result = errors.Join(result, err)
		}
		return result
	}, nil
}
