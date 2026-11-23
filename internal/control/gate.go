package control

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const gateFile = "gate.json"

// GateState is the persisted delivery decision.
type GateState struct {
	Paused      bool   `json:"paused"`
	Reason      string `json:"reason"`
	Actor       string `json:"actor"`
	ChangedAtMS int64  `json:"changed_at_ms"`
}

// Gate holds ready plans while paused. It survives restarts so a crash or
// redeploy cannot silently resume delivery. A nil gate never pauses.
type Gate struct {
	path   string
	paused atomic.Bool
	mu     sync.Mutex
	state  GateState
}

// OpenGate restores dir's gate. A file that cannot be read stops startup:
// guessing "not paused" could undo an operator's stop decision.
func OpenGate(dir string) (*Gate, error) {
	gate := &Gate{path: filepath.Join(dir, gateFile)}
	encoded, err := os.ReadFile(gate.path)
	if errors.Is(err, os.ErrNotExist) {
		return gate, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read delivery gate: %w", err)
	}
	if err := json.Unmarshal(encoded, &gate.state); err != nil {
		return nil, fmt.Errorf("decode delivery gate: %w", err)
	}
	gate.paused.Store(gate.state.Paused)
	return gate, nil
}

// Paused is an atomic load, safe on every plan delivery.
func (g *Gate) Paused() bool { return g != nil && g.paused.Load() }

func (g *Gate) State() GateState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.state
}

// Set persists the decision. Pausing takes effect even if the write fails,
// and resuming only after the write succeeds, so an I/O error fails closed.
func (g *Gate) Set(paused bool, actor, reason string, now time.Time) (GateState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	next := GateState{Paused: paused, Reason: reason, Actor: actor, ChangedAtMS: now.UnixMilli()}
	if paused {
		g.paused.Store(true)
		g.state = next
	}
	if err := writeDurable(g.path, next); err != nil {
		return g.state, fmt.Errorf("persist delivery gate: %w", err)
	}
	g.state = next
	g.paused.Store(paused)
	slog.Warn("delivery gate changed", "paused", paused, "actor", actor, "reason", reason)
	return next, nil
}

func writeDurable(path string, value any) (err error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".gate-*")
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			os.Remove(file.Name())
		}
	}()
	if _, err = file.Write(encoded); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

type gateKey struct{}

func WithGate(ctx context.Context, gate *Gate) context.Context {
	return context.WithValue(ctx, gateKey{}, gate)
}

func FromContext(ctx context.Context) *Gate {
	gate, _ := ctx.Value(gateKey{}).(*Gate)
	return gate
}
