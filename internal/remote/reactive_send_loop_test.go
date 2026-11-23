package remote

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gofrs/flock"
)

// A queued job is executed by the send loop and its record reaches the output
// through the writer, including one finished just before shutdown.
func TestSendLoopHandsRecordsToTheWriter(t *testing.T) {
	r, event := reactiveFixture(t)
	s := r.reactive
	s.observations, s.diagnostics, s.done = make(chan any, 4), make(chan any, 4), make(chan struct{})
	s.results = make(chan map[string]any, reactiveResultQueue)
	path := filepath.Join(t.TempDir(), "reactive.jsonl")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	lock := flock.New(path + ".lock")
	if err = lock.Lock(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	go s.run(ctx, file, lock)
	if err := r.offerReactive(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(raw, []byte(`"status":"shadow_admitted"`)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("intent record not written: %s", raw)
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-s.done:
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not stop")
	}
}
