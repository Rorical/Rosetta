package control

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
)

// The socket and SIGHUP both reach the runtime through one channel; the
// socket caller hears the outcome, and the status follows each new run.
func TestRotationRequestsReachRuntime(t *testing.T) {
	dir := shortTempDir(t)
	cfg := config.Config{ControlDir: dir, RunRoot: "state", Network: config.Network{Name: "optimism", ChainID: 10}}
	ctx, closeControl, err := Start(context.Background(), cfg, "rpc")
	if err != nil {
		t.Fatal(err)
	}
	defer closeControl()
	requests := Rotations(ctx)
	if requests == nil {
		t.Fatal("run root without rotation requests")
	}
	first, err := StartRun(ctx, "state/runs/a/candidates.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	handled := make(chan string, 2)
	go func() {
		for _, run := range []string{"b", "c"} {
			request := <-requests
			path := filepath.Join("state", "runs", run, "candidates.jsonl")
			id, err := StartRun(ctx, path)
			request.Finish(RotationResult{Closed: "previous", Run: filepath.Dir(path), RunID: id}, err)
			handled <- id
		}
	}()

	client := socketClient(filepath.Join(dir, socketFile))
	defer client.CloseIdleConnections()
	response, err := client.Post("http://rosetta/rotate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result RotationResult
	json.NewDecoder(response.Body).Decode(&result)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || result.RunID == "" || result.RunID == first || <-handled != result.RunID {
		t.Fatalf("socket rotation: %d %+v", response.StatusCode, result)
	}
	if status := ctx.Value(boardKey{}).(*board).read(); status.RunID != result.RunID || !filepath.IsAbs(status.Outputs.Candidates) || filepath.Base(filepath.Dir(status.Outputs.Candidates)) != "b" {
		t.Fatalf("status after rotation: %+v", status)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGHUP); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-handled:
		if id == result.RunID {
			t.Fatal("hangup reused the run ID")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SIGHUP did not request a rotation")
	}
}

func TestRotationRefusedWithoutRunRoot(t *testing.T) {
	gate, err := OpenGate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	NewHandler(gate, nil, Status{}).ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/rotate", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("rotation without run root: %d", response.Code)
	}
	if _, err := requestRotation(canceled(), make(chan Rotation)); !errors.Is(err, context.Canceled) {
		t.Fatal("a stopped runtime left the requester waiting", err)
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
