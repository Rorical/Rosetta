package control

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// rotationTimeout bounds how long POST /rotate waits for the closed run's
// files. A rotation that outlasts it still completes; only the reply is lost.
const rotationTimeout = time.Minute

// Rotation asks the runtime to close the current run's outputs and continue in
// a new run directory without restarting.
type Rotation struct {
	reply chan rotationReply
}

// RotationResult names the run that closed and the one now receiving output.
type RotationResult struct {
	Closed string `json:"closed"`
	Run    string `json:"run"`
	RunID  string `json:"run_id"`
}

type rotationReply struct {
	result RotationResult
	err    error
}

// Finish reports the outcome once the closed run's files are complete. It
// never blocks, even when the requester has given up.
func (r Rotation) Finish(result RotationResult, err error) {
	r.reply <- rotationReply{result, err}
}

type rotationsKey struct{}

// Rotations delivers operator rotation requests. Without -run-root it is nil
// and never ready.
func Rotations(ctx context.Context) <-chan Rotation {
	requests, _ := ctx.Value(rotationsKey{}).(chan Rotation)
	return requests
}

// StartRun publishes a run's candidate output under a fresh run ID, so the
// console finishes the previous file and follows the new one. StartedAtMS
// stays at process start: counters and execution summaries span runs.
func StartRun(ctx context.Context, candidates string) (string, error) {
	b, _ := ctx.Value(boardKey{}).(*board)
	if b == nil {
		return "", nil
	}
	absolute, err := filepath.Abs(candidates)
	if err != nil {
		return "", fmt.Errorf("resolve output path: %w", err)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	// The console retires a file once its run ID is no longer current, so IDs
	// must never repeat, even for two runs started within one millisecond.
	b.lastRunMS = max(time.Now().UnixMilli(), b.lastRunMS+1, b.status.StartedAtMS+1)
	b.status.RunID = newRunID(b.lastRunMS)
	b.status.Outputs.Candidates = absolute
	return b.status.RunID, nil
}

func newRunID(milliseconds int64) string {
	return fmt.Sprintf("%d-%d", milliseconds, os.Getpid())
}

// requestRotation hands one rotation to the runtime and waits for its outcome.
func requestRotation(ctx context.Context, requests chan<- Rotation) (RotationResult, error) {
	rotation := Rotation{reply: make(chan rotationReply, 1)}
	select {
	case requests <- rotation:
	case <-ctx.Done():
		return RotationResult{}, ctx.Err()
	}
	select {
	case reply := <-rotation.reply:
		return reply.result, reply.err
	case <-ctx.Done():
		return RotationResult{}, ctx.Err()
	}
}

func serveRotation(requests chan<- Rotation) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if requests == nil {
			http.Error(w, "rotation requires -run-root", http.StatusConflict)
			return
		}
		// The server's write timeout suits status reads, not a rotation that
		// syncs files and encodes the pool snapshot.
		http.NewResponseController(w).SetWriteDeadline(time.Now().Add(rotationTimeout + 5*time.Second))
		ctx, cancel := context.WithTimeout(r.Context(), rotationTimeout)
		defer cancel()
		result, err := requestRotation(ctx, requests)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, result)
	}
}

// watchHangup turns SIGHUP into a rotation until the returned function runs.
// The runtime logs each outcome; one rotation runs at a time.
func watchHangup(requests chan<- Rotation) func() {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGHUP)
	ctx, cancel := context.WithCancel(context.Background())
	var worker sync.WaitGroup
	worker.Go(func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-signals:
				requestRotation(ctx, requests)
			}
		}
	})
	return func() {
		signal.Stop(signals)
		cancel()
		worker.Wait()
	}
}
