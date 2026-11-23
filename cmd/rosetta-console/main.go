// Command rosetta-console serves the operator dashboard for running instances.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/Rorical/Rosetta/internal/console"
	"github.com/Rorical/Rosetta/web"
)

type hostFlags []string

func (f *hostFlags) String() string { return strings.Join(*f, ",") }

func (f *hostFlags) Set(value string) error {
	*f = append(*f, value)
	return nil
}

type instanceFlags []console.Instance

func (f *instanceFlags) String() string { return fmt.Sprint(*f) }

func (f *instanceFlags) Set(value string) error {
	name, dir, ok := strings.Cut(value, "=")
	if !ok || !validName(name) || dir == "" {
		return errors.New("want name=control-dir, with a name of lowercase letters, digits and dashes")
	}
	*f = append(*f, console.Instance{Name: name, Dir: dir})
	return nil
}

// Names reach PromQL label matchers and URLs, so the alphabet stays small.
func validName(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '-' {
			return false
		}
	}
	return true
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil && !errors.Is(err, flag.ErrHelp) {
		slog.Error("console stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	flags := flag.NewFlagSet("rosetta-console", flag.ContinueOnError)
	listen := flags.String("listen", "127.0.0.1:8788", "loopback address to serve the console on")
	dataDir := flags.String("data-dir", "", "directory for the console database")
	prometheus := flags.String("prometheus", "", "Prometheus base URL for history and alerts")
	var instances instanceFlags
	flags.Var(&instances, "instance", "name=control-dir of one bot; repeat for each instance")
	var publicHosts hostFlags
	flags.Var(&publicHosts, "public-host", "hostname a gateway serves this console under; repeat as needed")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if len(instances) == 0 || *dataDir == "" {
		return errors.New("-instance and -data-dir are required")
	}
	if err := requireLoopback(*listen); err != nil {
		return err
	}
	auth, err := console.NewAuth(os.Getenv("ROSETTA_CONSOLE_TOKEN"))
	if err != nil {
		return fmt.Errorf("ROSETTA_CONSOLE_TOKEN: %w", err)
	}
	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	store, err := console.OpenStore(ctx, filepath.Join(*dataDir, "console.db"))
	if err != nil {
		return err
	}
	defer store.Close()

	hub := console.NewHub(store, instances)
	go hub.Run(ctx)
	// No WriteTimeout: the event stream is long-lived and sets per-write deadlines.
	server := &http.Server{
		Addr:              *listen,
		Handler:           console.NewServer(hub, store, auth, console.NewPrometheus(*prometheus), web.Dist(), publicHosts).Handler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	failed := make(chan error, 1)
	go func() { failed <- server.ListenAndServe() }()
	slog.Info("console listening", "address", *listen, "instances", len(instances))
	select {
	case err := <-failed:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		// Event streams never finish on their own; Close ends them after the grace period.
		if err := server.Shutdown(shutdown); err != nil {
			return server.Close()
		}
		return nil
	}
}

// Remote access goes through an SSH tunnel or an authenticating gateway; the
// console itself speaks plain HTTP and must not face a network directly.
func requireLoopback(address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid -listen address: %w", err)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return fmt.Errorf("-listen must be a loopback address, got %s", host)
	}
	return nil
}
