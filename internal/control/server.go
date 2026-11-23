package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gofrs/flock"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

const (
	socketFile = "ops.sock"
	lockFile   = "control.lock"
	// sun_path holds 104 bytes on Darwin and 108 on Linux, including the NUL.
	maxSocketPath  = 103
	maxReasonBytes = 200
)

// Outputs lists the append-only JSONL files the console tails. Candidates are
// a new file per run, known at startup. The reactive files are appended across
// runs and announced once the reactive runtime has opened them.
type Outputs struct {
	Candidates  string `json:"candidates,omitempty"`
	Reactive    string `json:"reactive,omitempty"`
	Competition string `json:"competition,omitempty"`
}

// Status identifies one run. RunID changes on every start and, under
// -run-root, on every rotation.
type Status struct {
	RunID       string  `json:"run_id"`
	Network     string  `json:"network"`
	ChainID     uint64  `json:"chain_id"`
	Backend     string  `json:"backend"`
	EventSource string  `json:"event_source"`
	Version     string  `json:"version"`
	StartedAtMS int64   `json:"started_at_ms"`
	Outputs     Outputs `json:"outputs"`
}

// Start opens metric sinks and, with cfg.ControlDir set, the operator socket.
// The returned context carries both. Close after workers join.
func Start(ctx context.Context, cfg config.Config, backend string) (context.Context, func() error, error) {
	build := telemetry.Build{Version: buildVersion(), Backend: backend, Network: cfg.Network.Name, ChainID: cfg.ChainID()}
	// Under -run-root the runtime writes each run's final metrics into that
	// run's directory, so the sink has no fixed output file.
	registry, closeMetrics, err := telemetry.Start(telemetry.Options{Listen: cfg.MetricsListen, Output: cfg.MetricsOut, Collect: cfg.ControlDir != "", Build: build})
	if err != nil {
		return ctx, nil, err
	}
	stopProfiling, err := telemetry.StartProfiling(cfg.ProfileListen)
	if err != nil {
		return ctx, nil, errors.Join(err, closeMetrics())
	}
	ctx = telemetry.WithRegistry(ctx, registry)
	registry.Set(telemetry.Standard, telemetry.RPCCallBudget, int64(cfg.RPCMaxCalls))
	closeSinks := func() error {
		return errors.Join(stopProfiling(), closeMetrics())
	}
	if cfg.ControlDir == "" {
		return ctx, closeSinks, nil
	}
	status, err := newStatus(cfg, build, time.Now())
	if err != nil {
		return ctx, nil, errors.Join(err, closeSinks())
	}
	var rotations chan Rotation
	if cfg.RunRoot != "" {
		rotations = make(chan Rotation)
	}
	server, err := listen(cfg.ControlDir, registry, status, rotations)
	if err != nil {
		return ctx, nil, errors.Join(err, closeSinks())
	}
	ctx = context.WithValue(WithGate(ctx, server.gate), boardKey{}, server.board)
	stopHangup := func() {}
	if rotations != nil {
		ctx = context.WithValue(ctx, rotationsKey{}, rotations)
		stopHangup = watchHangup(rotations)
	}
	return ctx, func() error {
		stopHangup()
		return errors.Join(server.close(), closeSinks())
	}, nil
}

func newStatus(cfg config.Config, build telemetry.Build, now time.Time) (Status, error) {
	status := Status{
		RunID: newRunID(now.UnixMilli()), Network: build.Network, ChainID: build.ChainID,
		Backend: build.Backend, EventSource: cfg.EventSource, Version: build.Version, StartedAtMS: now.UnixMilli(),
	}
	if cfg.CandidateOut != "" {
		absolute, err := filepath.Abs(cfg.CandidateOut)
		if err != nil {
			return Status{}, fmt.Errorf("resolve output path: %w", err)
		}
		status.Outputs.Candidates = absolute
	}
	return status, nil
}

// board is the served status. Outputs grow after startup, and each new run
// replaces the run ID and candidate output.
type board struct {
	mu        sync.Mutex
	status    Status
	lastRunMS int64
}

func (b *board) read() Status {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.status
}

type boardKey struct{}

// AnnounceOutputs publishes files a component opened after startup, such as
// the reactive output its policy names. Empty fields keep earlier values.
// Without a control socket nothing reads them and the call does nothing.
func AnnounceOutputs(ctx context.Context, outputs Outputs) error {
	b, _ := ctx.Value(boardKey{}).(*board)
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, output := range []struct {
		from string
		to   *string
	}{{outputs.Reactive, &b.status.Outputs.Reactive}, {outputs.Competition, &b.status.Outputs.Competition}} {
		if output.from == "" {
			continue
		}
		absolute, err := filepath.Abs(output.from)
		if err != nil {
			return fmt.Errorf("resolve output path: %w", err)
		}
		*output.to = absolute
	}
	return nil
}

// version is set at link time. Workspace builds, which every native binary is,
// carry no VCS stamp in their build info.
var version string

func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	revision, dirty := "", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	if revision == "" {
		return "unknown"
	}
	revision = revision[:min(len(revision), 12)]
	if dirty {
		return revision + "-dirty"
	}
	return revision
}

type server struct {
	gate     *Gate
	board    *board
	lock     *flock.Flock
	http     *http.Server
	finished chan error
}

func listen(dir string, registry *telemetry.Registry, status Status, rotations chan<- Rotation) (*server, error) {
	socket := filepath.Join(dir, socketFile)
	if len(socket) > maxSocketPath {
		return nil, fmt.Errorf("control socket path exceeds %d bytes: %s", maxSocketPath, socket)
	}
	if err := prepareDir(dir); err != nil {
		return nil, err
	}
	lock := flock.New(filepath.Join(dir, lockFile))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("lock control directory: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("control directory %s is held by another instance", dir)
	}
	gate, err := OpenGate(dir)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	// The lock proves any remaining socket file belongs to a dead process.
	if err := os.Remove(socket); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, errors.Join(fmt.Errorf("remove stale control socket: %w", err), lock.Close())
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		return nil, errors.Join(fmt.Errorf("listen on control socket: %w", err), lock.Close())
	}
	registry.Sample(telemetry.Standard, telemetry.DeliveryPaused, func() int64 {
		if gate.Paused() {
			return 1
		}
		return 0
	})
	s := &server{gate: gate, board: &board{status: status}, lock: lock, finished: make(chan error, 1)}
	s.http = &http.Server{Handler: newHandler(gate, registry, s.board, rotations), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second}
	go func() { s.finished <- s.http.Serve(listener) }()
	return s, nil
}

// prepareDir refuses a directory other users can enter: anyone who can reach
// the socket can stop or resume plan delivery.
func prepareDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create control directory: %w", err)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("inspect control directory: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("control directory %s must be a real directory, not a link", dir)
	}
	if owner, ok := info.Sys().(*syscall.Stat_t); !ok || int(owner.Uid) != os.Geteuid() {
		return fmt.Errorf("control directory %s must belong to the running user", dir)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("control directory %s must not be accessible to group or others", dir)
	}
	return nil
}

func (s *server) close() error {
	result := s.http.Close()
	if err := <-s.finished; !errors.Is(err, http.ErrServerClosed) {
		result = errors.Join(result, err)
	}
	return errors.Join(result, s.lock.Close())
}

type gateRequest struct {
	Paused bool   `json:"paused"`
	Actor  string `json:"actor"`
	Reason string `json:"reason"`
}

// NewHandler serves the local operator API with a fixed status. The socket
// directory authorizes callers, so requests carry no credentials.
func NewHandler(gate *Gate, registry *telemetry.Registry, status Status) http.Handler {
	return newHandler(gate, registry, &board{status: status}, nil)
}

func newHandler(gate *Gate, registry *telemetry.Registry, status *board, rotations chan<- Rotation) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", registry)
	mux.Handle("GET /healthz", registry)
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, status.read()) })
	mux.HandleFunc("GET /snapshot", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, registry.Snapshot()) })
	mux.HandleFunc("GET /gate", func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, http.StatusOK, gate.State()) })
	mux.HandleFunc("POST /rotate", serveRotation(rotations))
	mux.HandleFunc("PUT /gate", func(w http.ResponseWriter, r *http.Request) {
		var request gateRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil {
			http.Error(w, "invalid gate request", http.StatusBadRequest)
			return
		}
		request.Reason = strings.TrimSpace(request.Reason)
		if request.Actor == "" || request.Reason == "" || len(request.Reason) > maxReasonBytes {
			http.Error(w, "actor and a reason up to 200 bytes are required", http.StatusBadRequest)
			return
		}
		state, err := gate.Set(request.Paused, request.Actor, request.Reason, time.Now())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, state)
	})
	return mux
}

func writeJSON(w http.ResponseWriter, code int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(value)
}
