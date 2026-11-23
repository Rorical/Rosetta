package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/backtest"
	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/pipeline"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func controlClient(dir string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "ops.sock"))
	}}}
}

func firstLine(t *testing.T, path string, into any) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	if !scanner.Scan() {
		t.Fatalf("%s is empty", path)
	}
	if err := json.Unmarshal(scanner.Bytes(), into); err != nil {
		t.Fatal(err)
	}
}

// A rotation closes the run at the state the process holds, without a restart:
// the closed run gets the files the start script and finish step relied on,
// and the next run starts self-contained while the console sees its new path.
func TestRunRootRotatesWithoutRestart(t *testing.T) {
	store, pools := poolFixture(t)
	server, next := canonicalServer(t, store.Block(), pools)
	directory := t.TempDir()
	snapshotPath := filepath.Join(directory, "resume.json")
	data, _ := store.EncodeSnapshot()
	os.WriteFile(snapshotPath, data, 0o600)
	searchPath := filepath.Join(directory, "search-config.json")
	writeJSON(t, searchPath, map[string]any{"k": 1, "max_depth": 2, "max_cycles": 100, "funding": []any{map[string]any{"token": addressAt(1).Hex(), "min_input": "1", "seed_input": "1000", "max_input": "100000"}}})
	controlDir, err := os.MkdirTemp("/tmp", "rrot")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(controlDir) })
	state := filepath.Join(directory, "state")
	cfg, err := config.Parse([]string{"-rpc", server.URL, "-rpc-poll", "20ms", "-pool-snapshot", snapshotPath, "-search-config", searchPath, "-run-root", state, "-control-dir", controlDir}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := (Factory{}).Create(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	stopped := make(chan error, 1)
	go func() { stopped <- backend.Run(ctx) }()

	client := controlClient(controlDir)
	defer client.CloseIdleConnections()
	status := func() control.Status {
		t.Helper()
		var status control.Status
		response, err := client.Get("http://rosetta/status")
		if err != nil {
			return status
		}
		defer response.Body.Close()
		json.NewDecoder(response.Body).Decode(&status)
		return status
	}
	eventually := func(what string, check func() bool) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); !check(); time.Sleep(20 * time.Millisecond) {
			if time.Now().After(deadline) {
				t.Fatal("timed out waiting for " + what)
			}
		}
	}
	var first control.Status
	eventually("the first run", func() bool { first = status(); return first.Outputs.Candidates != "" })
	firstRun := filepath.Dir(first.Outputs.Candidates)
	eventually("the first candidate", func() bool {
		written, _ := os.ReadFile(first.Outputs.Candidates)
		return strings.Contains(string(written), `"status":"candidate"`)
	})

	response, err := client.Post("http://rosetta/rotate", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	var result control.RotationResult
	json.NewDecoder(response.Body).Decode(&result)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || result.Closed != firstRun || filepath.Dir(result.Run) != filepath.Join(state, "runs") || result.Run == firstRun {
		t.Fatalf("rotation: %d %+v", response.StatusCode, result)
	}
	second := status()
	if second.RunID == first.RunID || second.RunID != result.RunID || second.Outputs.Candidates != filepath.Join(result.Run, "candidates.jsonl") || second.StartedAtMS != first.StartedAtMS {
		t.Fatalf("status after rotation: %+v, before %+v", second, first)
	}
	if marker, _ := os.ReadFile(filepath.Join(state, "current-run")); string(marker) != result.Run+"\n" {
		t.Fatalf("current-run marker %q", marker)
	}

	// The closed run is complete before the reply: the snapshot loads at the
	// processed block, metrics are written, and both outputs open as readers expect.
	encoded, err := os.ReadFile(filepath.Join(firstRun, "final-pools.json"))
	if err != nil {
		t.Fatal(err)
	}
	final := poolstate.New()
	if err := final.LoadSnapshot(encoded); err != nil || final.Block() != next {
		t.Fatalf("closed run snapshot at %+v: %v", final.Block(), err)
	}
	var metrics map[string]any
	firstLine(t, filepath.Join(firstRun, "final-metrics.json"), &metrics)
	if metrics["schema_version"] != float64(1) {
		t.Fatalf("closed run metrics: %v", metrics)
	}
	for _, run := range []string{firstRun, result.Run} {
		var replay backtest.Record
		firstLine(t, filepath.Join(run, "replay.jsonl"), &replay)
		var session pipeline.ResearchEvent
		firstLine(t, filepath.Join(run, "observations.jsonl"), &session)
		if replay.Type != "snapshot" || session.Type != "session" {
			t.Fatalf("%s opens with %s replay and %s observation records", run, replay.Type, session.Type)
		}
		if copied, err := os.ReadFile(filepath.Join(run, "search.json")); err != nil || len(copied) == 0 {
			t.Fatalf("%s lacks its search configuration", run)
		}
	}

	cancel()
	if err := <-stopped; err != nil && !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, name := range []string{"final-pools.json", "final-metrics.json"} {
		if _, err := os.Stat(filepath.Join(result.Run, name)); err != nil {
			t.Fatalf("shutdown did not close the current run: %v", err)
		}
	}
}
