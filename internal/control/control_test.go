package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func TestGateSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	gate, err := OpenGate(dir)
	if err != nil || gate.Paused() {
		t.Fatalf("new gate: paused=%v err=%v", gate.Paused(), err)
	}
	if _, err := gate.Set(true, "operator", "incident", time.UnixMilli(1000)); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenGate(dir)
	if err != nil || !reopened.Paused() {
		t.Fatalf("restart lost pause: paused=%v err=%v", reopened.Paused(), err)
	}
	if state := reopened.State(); state.Reason != "incident" || state.Actor != "operator" || state.ChangedAtMS != 1000 {
		t.Fatalf("restored state: %+v", state)
	}
}

func TestGateFailsClosed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, gateFile), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenGate(dir); err == nil {
		t.Fatal("corrupt gate must stop startup")
	}

	gate := &Gate{path: filepath.Join(t.TempDir(), "missing", gateFile)}
	if _, err := gate.Set(true, "operator", "incident", time.Now()); err == nil || !gate.Paused() {
		t.Fatalf("failed write must still pause: paused=%v err=%v", gate.Paused(), err)
	}
	if _, err := gate.Set(false, "operator", "resume", time.Now()); err == nil || !gate.Paused() {
		t.Fatalf("failed write must not resume: paused=%v err=%v", gate.Paused(), err)
	}
}

func TestNilGateNeverPauses(t *testing.T) {
	var gate *Gate
	if gate.Paused() || FromContext(context.Background()) != nil {
		t.Fatal("absent gate must allow delivery")
	}
}

func TestGateRequestValidation(t *testing.T) {
	gate, err := OpenGate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHandler(gate, &telemetry.Registry{}, Status{RunID: "run"})
	for _, test := range []struct {
		name, method, path, body string
		code                     int
	}{
		{"missing reason", http.MethodPut, "/gate", `{"paused":true,"actor":"operator"}`, http.StatusBadRequest},
		{"blank reason", http.MethodPut, "/gate", `{"paused":true,"actor":"operator","reason":"  "}`, http.StatusBadRequest},
		{"missing actor", http.MethodPut, "/gate", `{"paused":true,"reason":"x"}`, http.StatusBadRequest},
		{"oversized reason", http.MethodPut, "/gate", `{"paused":true,"actor":"operator","reason":"` + strings.Repeat("x", 201) + `"}`, http.StatusBadRequest},
		{"unknown field", http.MethodPut, "/gate", `{"paused":true,"actor":"operator","reason":"x","force":true}`, http.StatusBadRequest},
		{"malformed", http.MethodPut, "/gate", `{`, http.StatusBadRequest},
		{"wrong method", http.MethodPost, "/gate", `{}`, http.StatusMethodNotAllowed},
		{"accepted", http.MethodPut, "/gate", `{"paused":true,"actor":"operator","reason":"incident"}`, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, httptest.NewRequest(test.method, test.path, strings.NewReader(test.body)))
			if response.Code != test.code {
				t.Fatalf("status %d: %s", response.Code, response.Body)
			}
		})
	}
	if !gate.Paused() {
		t.Fatal("accepted request did not pause")
	}
}

func socketClient(path string) *http.Client {
	return &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", path)
	}}}
}

// macOS temp paths exceed sun_path, so sockets use a short directory.
func shortTempDir(t *testing.T) string {
	dir, err := os.MkdirTemp("/tmp", "rctl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, "run")
}

func TestStartServesSocketAndHoldsOneInstance(t *testing.T) {
	dir := shortTempDir(t)
	cfg := config.Config{ControlDir: dir, CandidateOut: "candidates.jsonl", RPCMaxCalls: 9, Network: config.Network{Name: "arbitrum-one", ChainID: 42161}}
	ctx, closeControl, err := Start(context.Background(), cfg, "rpc")
	if err != nil {
		t.Fatal(err)
	}
	gate := FromContext(ctx)
	registry := telemetry.FromContext(ctx)
	if gate == nil || registry == nil {
		t.Fatal("context lacks gate or registry")
	}
	if _, _, err := Start(context.Background(), cfg, "rpc"); err == nil {
		t.Fatal("second instance acquired the control directory")
	}
	if err := AnnounceOutputs(ctx, Outputs{Reactive: "reactive.jsonl"}); err != nil {
		t.Fatal(err)
	}
	if err := AnnounceOutputs(ctx, Outputs{Competition: "reactive.jsonl.competition.jsonl"}); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, socketFile)
	client := socketClient(socket)
	var status Status
	response, err := client.Get("http://rosetta/status")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if err := json.NewDecoder(response.Body).Decode(&status); err != nil {
		t.Fatal(err)
	}
	// Announcements made after startup accumulate in the served status.
	if status.Backend != "rpc" || status.ChainID != 42161 || !filepath.IsAbs(status.Outputs.Candidates) || !filepath.IsAbs(status.Outputs.Reactive) || !filepath.IsAbs(status.Outputs.Competition) || status.RunID == "" {
		t.Fatalf("status: %+v", status)
	}

	body, _ := json.Marshal(gateRequest{Paused: true, Actor: "operator", Reason: "incident"})
	request, _ := http.NewRequest(http.MethodPut, "http://rosetta/gate", bytes.NewReader(body))
	put, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	put.Body.Close()
	if put.StatusCode != http.StatusOK || !gate.Paused() {
		t.Fatalf("gate over socket: %d", put.StatusCode)
	}
	snapshot := registry.Snapshot().Sources["standard"].Gauges
	if snapshot["delivery_paused"] != 1 || snapshot["rpc_call_budget"] != 9 {
		t.Fatalf("gauges: %+v", snapshot)
	}
	client.CloseIdleConnections()
	if err := closeControl(); err != nil {
		t.Fatal(err)
	}

	// A killed process leaves its socket file behind; the lock is what decides.
	ctx, closeControl, err = Start(context.Background(), cfg, "rpc")
	if err != nil {
		t.Fatalf("restart over stale socket: %v", err)
	}
	defer closeControl()
	if !FromContext(ctx).Paused() {
		t.Fatal("restart resumed delivery")
	}
}

func TestStartRejectsExposedDirectoryAndLongSocket(t *testing.T) {
	exposed := shortTempDir(t)
	if err := os.Mkdir(exposed, 0o755); err != nil {
		t.Fatal(err)
	}
	_, _, err := Start(context.Background(), config.Config{ControlDir: exposed}, "rpc")
	if err == nil || !strings.Contains(err.Error(), "accessible") {
		t.Fatalf("exposed directory: %v", err)
	}
	target := shortTempDir(t)
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(filepath.Dir(exposed), "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, _, err = Start(context.Background(), config.Config{ControlDir: link}, "rpc")
	if err == nil || !strings.Contains(err.Error(), "real directory") {
		t.Fatalf("symlinked directory: %v", err)
	}
	_, _, err = Start(context.Background(), config.Config{ControlDir: "/tmp/" + strings.Repeat("x", 120)}, "rpc")
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("long socket path: %v", err)
	}
	if errors.Is(err, os.ErrNotExist) {
		t.Fatal("long path reached the filesystem")
	}
}

func TestStartWithoutSinksIsDisabled(t *testing.T) {
	ctx, closeControl, err := Start(context.Background(), config.Config{}, "rpc")
	if err != nil {
		t.Fatal(err)
	}
	if telemetry.FromContext(ctx) != nil || FromContext(ctx) != nil {
		t.Fatal("disabled run created a registry or gate")
	}
	if err := AnnounceOutputs(ctx, Outputs{Reactive: "reactive.jsonl"}); err != nil {
		t.Fatal(err)
	}
	if err := closeControl(); err != nil {
		t.Fatal(err)
	}
}
