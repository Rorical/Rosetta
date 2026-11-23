package bidmodel

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"time"
)

// Store swaps whole immutable artifacts. No file I/O occurs in Snapshot or
// Predict. A bad replacement retains the last valid model only until expiry.
type Store struct{ current atomic.Pointer[Artifact] }

func (s *Store) Load(path string, scope Scope, now time.Time) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, MaxArtifactBytes+1))
	if err != nil {
		return err
	}
	model, err := Decode(data, scope)
	if err != nil {
		return err
	}
	if now.UnixMilli() < model.CreatedMS || now.UnixMilli() >= model.ExpiresMS {
		return fmt.Errorf("bid model is not currently valid")
	}
	s.current.Store(model)
	return nil
}

func (s *Store) Snapshot(now time.Time, apply bool) (*Artifact, string) {
	m := s.current.Load()
	if m == nil {
		return nil, "model_unavailable"
	}
	if now.UnixMilli() < m.CreatedMS || now.UnixMilli() >= m.ExpiresMS {
		return nil, "model_expired"
	}
	if apply && !m.Approved {
		return nil, "model_unapproved"
	}
	return m, ""
}

// Watch is called on a dedicated lifecycle goroutine; every decision keeps one
// snapshot even when a new file is promoted concurrently.
func (s *Store) Watch(ctx context.Context, path string, scope Scope, report func(error)) {
	timer := time.NewTicker(10 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-timer.C:
			report(s.Load(path, scope, now))
		}
	}
}
