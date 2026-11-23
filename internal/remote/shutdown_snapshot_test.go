package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	poolstate "github.com/Rorical/Rosetta/internal/state"
)

// Shutdown can surface as an error from any handler. The snapshot must still be
// written, or the next start replays every block since the previous one.
func TestShutdownWritesSnapshotWhenHandlerReturnsCancellation(t *testing.T) {
	store, _ := poolFixture(t)
	path := filepath.Join(t.TempDir(), "final.json")
	r := &runtime{store: store}
	r.cfg.PoolSnapshotOut = path
	running := t.Context()
	if err := r.persistOnShutdown(running, errors.New("handler failed")); err == nil {
		t.Fatal("handler error lost")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("snapshot written for a failure that is not a shutdown")
	}
	stopped, cancel := context.WithCancel(running)
	cancel()
	if err := r.persistOnShutdown(stopped, context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatal("no snapshot after shutdown through a handler", err)
	}
	if err := poolstate.New().LoadSnapshot(encoded); err != nil {
		t.Fatal(err)
	}
	if err := r.persistOnShutdown(stopped, nil); err != nil {
		t.Fatal("second shutdown write failed", err)
	}
}
