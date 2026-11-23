package console

import (
	"bytes"
	"context"
	"encoding/json"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(t.Context(), filepath.Join(t.TempDir(), "console.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
}

func TestQuantileInterpolatesAndReportsOverflow(t *testing.T) {
	buckets := []telemetry.Bucket{{UpperNS: 100, Count: 0}, {UpperNS: 200, Count: 50}, {UpperNS: 400, Count: 100}}
	for _, test := range []struct {
		name     string
		buckets  []telemetry.Bucket
		total    uint64
		q        float64
		want     uint64
		overflow bool
	}{
		{"bucket edge", buckets, 100, 0.5, 200, false},
		{"inside bucket", buckets, 100, 0.75, 300, false},
		{"first bucket from zero", []telemetry.Bucket{{UpperNS: 100, Count: 10}}, 10, 0.5, 50, false},
		{"samples above last bound", buckets, 110, 0.99, 400, true},
		{"skips empty lower buckets", []telemetry.Bucket{{UpperNS: 100, Count: 0}, {UpperNS: 200, Count: 0}, {UpperNS: 400, Count: 4}}, 4, 0.5, 300, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, overflow := quantile(test.buckets, test.total, test.q)
			if got != test.want || overflow != test.overflow {
				t.Fatalf("got %d overflow=%v", got, overflow)
			}
		})
	}
}

func TestApplySamplesRatesAndWindow(t *testing.T) {
	snapshot := func(messages, count, fast uint64, cpu float64) telemetry.Snapshot {
		return telemetry.Snapshot{Process: telemetry.Process{CPUSeconds: cpu}, Sources: map[string]telemetry.SourceSnapshot{"standard": {
			Counters: map[string]uint64{"messages": messages},
			Stages:   map[string]telemetry.Latency{"pool_apply": {Count: count, Buckets: []telemetry.Bucket{{UpperNS: 1000, Count: fast}, {UpperNS: 2000, Count: count}}}},
		}}}
	}
	start := time.Unix(1000, 0)
	view := InstanceView{}
	applySamples(&view, []sample{{start, snapshot(100, 10, 10, 1)}, {start.Add(10 * time.Second), snapshot(150, 110, 10, 3)}})
	source := view.Sources["standard"]
	if source.Rates["messages"] != 5 || view.CPUPercent != 20 {
		t.Fatalf("rates: %+v cpu=%v", source.Rates, view.CPUPercent)
	}
	stage := source.Stages["pool_apply"]
	// Lifetime samples sit in the fast bucket; only the window's slow ones count.
	if stage.Count != 100 || stage.Lifetime != 110 || stage.P50NS != 1500 {
		t.Fatalf("stage: %+v", stage)
	}
}

// skipLine is a reactive_skips record as the bot's writer encodes it.
func skipLine(t *testing.T, reason string, count uint64) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"type": "reactive_skips", "from_ms": 1000, "to_ms": 2000, "reasons": map[string]uint64{reason: count}})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded) + "\n"
}

func appendFile(t *testing.T, path, text string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(text); err != nil {
		t.Fatal(err)
	}
}

func skipsStored(t *testing.T, store *Store) map[string]uint64 {
	t.Helper()
	execution, err := store.Execution(t.Context(), "one", 0, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]uint64{}
	for _, skip := range execution.Skips {
		counts[skip.Reason] = skip.Count
	}
	return counts
}

func TestTailImportsOnlyCompleteLinesAndResumes(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "reactive.jsonl")
	if err := store.trackFile(ctx, tailFile{Path: path, Instance: "one", RunID: "run-1", Kind: kindReactive}); err != nil {
		t.Fatal(err)
	}
	first, second := skipLine(t, "a", 1), skipLine(t, "a", 2)
	appendFile(t, path, first+second[:20])
	tailer := &Tailer{store: store, instance: "one"}
	if err := tailer.Pass(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	if counts := skipsStored(t, store); counts["a"] != 1 {
		t.Fatalf("torn line imported: %+v", counts)
	}
	appendFile(t, path, second[20:]+"{not json}\n"+skipLine(t, "a", 4))
	// A new Tailer stands in for a console restart: the offset comes from the database.
	tailer = &Tailer{store: store, instance: "one"}
	if err := tailer.Pass(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	if err := tailer.Pass(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	if counts := skipsStored(t, store); counts["a"] != 7 {
		t.Fatalf("resume lost or repeated lines: %+v", counts)
	}
}

func TestTailKeepsFileUntilNewerRunIsKnown(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "reactive.jsonl")
	appendFile(t, path, skipLine(t, "a", 1))
	if err := store.trackFile(ctx, tailFile{Path: path, Instance: "one", RunID: "run-1", Kind: kindReactive}); err != nil {
		t.Fatal(err)
	}
	tailer := &Tailer{store: store, instance: "one"}
	for _, current := range []string{"", "run-1"} {
		if err := tailer.Pass(ctx, current); err != nil {
			t.Fatal(err)
		}
		if files, _ := store.openFiles(ctx, "one"); len(files) != 1 {
			t.Fatalf("file closed with current run %q", current)
		}
	}
	if err := tailer.Pass(ctx, "run-2"); err != nil {
		t.Fatal(err)
	}
	if files, _ := store.openFiles(ctx, "one"); len(files) != 0 {
		t.Fatal("retired file stayed open")
	}
}

// startFakeBot serves the real control handler on a real socket.
func startFakeBot(t *testing.T, status control.Status, registry *telemetry.Registry) (string, *control.Gate, func()) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rcon")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	gate, err := control.OpenGate(dir)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "ops.sock"))
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: control.NewHandler(gate, registry, status)}
	go server.Serve(listener)
	stop := func() { server.Close() }
	t.Cleanup(stop)
	return dir, gate, stop
}

type harness struct {
	server *Server
	store  *Store
	hub    *Hub
	gate   *control.Gate
	cookie *http.Cookie
	web    *httptest.Server

	stopBot func()
}

const testToken = "correct-horse-battery-staple"

func newHarness(t *testing.T, prometheus *Prometheus) *harness {
	t.Helper()
	store := testStore(t)
	registry := &telemetry.Registry{}
	registry.Add(telemetry.Standard, telemetry.Messages, 5)
	dir, gate, stop := startFakeBot(t, control.Status{RunID: "run-1", Network: "arbitrum-one"}, registry)
	hub := NewHub(store, []Instance{{Name: "one", Dir: dir}})
	auth, err := NewAuth(testToken)
	if err != nil {
		t.Fatal(err)
	}
	server := NewServer(hub, store, auth, prometheus, fs.FS(fstest.MapFS{"index.html": {Data: []byte("<title>console</title>")}, "assets/app.js": {Data: []byte("app")}}), []string{"console.example"})
	web := httptest.NewServer(server.Handler())
	t.Cleanup(web.Close)
	return &harness{server: server, store: store, hub: hub, gate: gate, web: web, stopBot: stop}
}

func (h *harness) do(t *testing.T, method, path, body string, mutate func(*http.Request)) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, h.web.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if method != http.MethodGet {
		request.Header.Set("Origin", h.web.URL)
	}
	if h.cookie != nil {
		request.AddCookie(h.cookie)
	}
	if mutate != nil {
		mutate(request)
	}
	response, err := http.DefaultTransport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { response.Body.Close() })
	return response
}

func (h *harness) signIn(t *testing.T) {
	t.Helper()
	response := h.do(t, http.MethodPost, "/api/session", `{"token":"`+testToken+`"}`, nil)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("sign in: %d", response.StatusCode)
	}
	cookies := response.Cookies()
	if len(cookies) != 1 || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie: %+v", cookies)
	}
	h.cookie = cookies[0]
}

func TestAPIRequiresSession(t *testing.T) {
	h := newHarness(t, nil)
	for _, path := range []string{"/api/instances", "/api/events", "/api/instances/one/sends", "/api/instances/one/pnl", "/api/instances/one/execution?since=0", "/api/instances/one/competition?range=1h", "/api/instances/one/assets", "/api/audit", "/api/alerts", "/api/instances/one/history/messages?range=1h"} {
		if response := h.do(t, http.MethodGet, path, "", nil); response.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: %d", path, response.StatusCode)
		}
	}
	if response := h.do(t, http.MethodPut, "/api/instances/one/gate", `{"paused":true,"reason":"x"}`, nil); response.StatusCode != http.StatusUnauthorized || h.gate.Paused() {
		t.Fatalf("gate without session: %d", response.StatusCode)
	}
	if response := h.do(t, http.MethodGet, "/", "", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("shell should load for the sign-in page: %d", response.StatusCode)
	}
}

func TestLoginSessionLifetimeAndLogout(t *testing.T) {
	h := newHarness(t, nil)
	now := time.Unix(1000, 0)
	h.server.now = func() time.Time { return now }
	if response := h.do(t, http.MethodPost, "/api/session", `{"token":"wrong"}`, nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", response.StatusCode)
	}
	// A failed guess must not delay the operator: signing in succeeds at once.
	h.signIn(t)
	if response := h.do(t, http.MethodGet, "/api/instances", "", nil); response.StatusCode != http.StatusOK {
		t.Fatalf("session rejected: %d", response.StatusCode)
	}
	now = now.Add(sessionLifetime + time.Second)
	if response := h.do(t, http.MethodGet, "/api/instances", "", nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expired session accepted: %d", response.StatusCode)
	}
	now = now.Add(-sessionLifetime)
	if response := h.do(t, http.MethodDelete, "/api/session", "", nil); response.StatusCode != http.StatusNoContent {
		t.Fatalf("sign out: %d", response.StatusCode)
	}
	if response := h.do(t, http.MethodGet, "/api/instances", "", nil); response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("session survived sign out: %d", response.StatusCode)
	}
}

func TestFailedLoginsShareAGlobalCap(t *testing.T) {
	h := newHarness(t, nil)
	started := time.Now()
	var attempts sync.WaitGroup
	for range loginSlots + 1 {
		attempts.Go(func() {
			request, err := http.NewRequest(http.MethodPost, h.web.URL+"/api/session", strings.NewReader(`{"token":"wrong"}`))
			if err != nil {
				t.Error(err)
				return
			}
			request.Header.Set("Origin", h.web.URL)
			response, err := http.DefaultTransport.RoundTrip(request)
			if err != nil {
				t.Error(err)
				return
			}
			response.Body.Close()
		})
	}
	attempts.Wait()
	if elapsed := time.Since(started); elapsed < 2*failedLoginDelay {
		t.Fatalf("%d concurrent guesses finished in %v", loginSlots+1, elapsed)
	}
	h.signIn(t)
}

func TestNewAuthRejectsShortToken(t *testing.T) {
	if _, err := NewAuth("short"); err == nil {
		t.Fatal("short token accepted")
	}
}

func TestCrossOriginWriteRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t)
	for _, test := range []struct {
		name   string
		mutate func(*http.Request)
	}{
		{"other origin", func(r *http.Request) { r.Header.Set("Origin", "http://evil.example") }},
		{"sibling port", func(r *http.Request) { r.Header.Set("Origin", "http://127.0.0.1:1") }},
		{"no origin", func(r *http.Request) { r.Header.Del("Origin") }},
	} {
		response := h.do(t, http.MethodPut, "/api/instances/one/gate", `{"paused":true,"reason":"x"}`, test.mutate)
		if response.StatusCode != http.StatusForbidden || h.gate.Paused() {
			t.Fatalf("%s: %d paused=%v", test.name, response.StatusCode, h.gate.Paused())
		}
	}
}

func TestUnrecognizedHostRefused(t *testing.T) {
	h := newHarness(t, nil)
	for host, want := range map[string]int{
		"evil.example":        http.StatusForbidden,
		"evil.example:8788":   http.StatusForbidden,
		"localhost:8788":      http.StatusOK,
		"127.0.0.1":           http.StatusOK,
		"[::1]:8788":          http.StatusOK,
		"console.example:443": http.StatusOK,
	} {
		response := h.do(t, http.MethodGet, "/", "", func(r *http.Request) { r.Host = host })
		if response.StatusCode != want {
			t.Errorf("%s: %d", host, response.StatusCode)
		}
	}
}

func TestEventStreamEndsAfterSignOut(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	go h.hub.Run(ctx)
	response := h.do(t, http.MethodGet, "/api/events", "", nil)
	buffer := make([]byte, 1<<16)
	if _, err := response.Body.Read(buffer); err != nil {
		t.Fatal(err)
	}
	h.server.auth.Logout(h.cookie.Value)
	finished := make(chan error, 1)
	go func() {
		for {
			if _, err := response.Body.Read(buffer); err != nil {
				finished <- err
				return
			}
		}
	}()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("stream kept running after sign out")
	}
}

func TestGateChangeIsAudited(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t)
	if response := h.do(t, http.MethodPut, "/api/instances/one/gate", `{"paused":true,"reason":"  "}`, nil); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("blank reason: %d", response.StatusCode)
	}
	if response := h.do(t, http.MethodPut, "/api/instances/missing/gate", `{"paused":true,"reason":"x"}`, nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown instance: %d", response.StatusCode)
	}
	if response := h.do(t, http.MethodPut, "/api/instances/one/gate", `{"paused":true,"reason":"incident"}`, nil); response.StatusCode != http.StatusOK || !h.gate.Paused() {
		t.Fatalf("pause: %d", response.StatusCode)
	}
	if state := h.gate.State(); state.Actor != operatorActor || state.Reason != "incident" {
		t.Fatalf("gate state: %+v", state)
	}
	h.stopBot()
	if response := h.do(t, http.MethodPut, "/api/instances/one/gate", `{"paused":false,"reason":"recovered"}`, nil); response.StatusCode != http.StatusBadGateway || !h.gate.Paused() {
		t.Fatalf("resume against dead socket: %d", response.StatusCode)
	}
	entries, err := h.store.Audit(t.Context(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Action != "resume_delivery" || entries[0].Outcome != outcomeFailed || entries[0].Error == "" ||
		entries[1].Action != "pause_delivery" || entries[1].Outcome != outcomeApplied || entries[1].Reason != "incident" {
		t.Fatalf("audit: %+v", entries)
	}
}

func TestGateRefusedWithoutAuditLog(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t)
	h.store.Close()
	if response := h.do(t, http.MethodPut, "/api/instances/one/gate", `{"paused":true,"reason":"incident"}`, nil); response.StatusCode != http.StatusInternalServerError || h.gate.Paused() {
		t.Fatalf("unaudited change: %d paused=%v", response.StatusCode, h.gate.Paused())
	}
}

func TestHubPublishesInstanceAndReportsOutage(t *testing.T) {
	h := newHarness(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	updates, unsubscribe := h.hub.Subscribe()
	defer unsubscribe()
	go h.hub.Run(ctx)
	waitFor := func(check func(InstanceView) bool) {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case payload := <-updates:
				var views []InstanceView
				if err := json.Unmarshal(payload, &views); err != nil {
					t.Fatal(err)
				}
				if check(views[0]) {
					return
				}
			case <-deadline:
				t.Fatal("expected view never arrived")
			}
		}
	}
	waitFor(func(v InstanceView) bool {
		return v.Online && v.Status.RunID == "run-1" && v.Sources["standard"].Counters["messages"] == 5
	})
	h.stopBot()
	waitFor(func(v InstanceView) bool { return !v.Online && v.Error != "" && v.Status != nil })
}

func TestEventStreamSendsCurrentViewFirst(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t)
	response := h.do(t, http.MethodGet, "/api/events", "", nil)
	if response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("content type: %s", response.Header.Get("Content-Type"))
	}
	buffer := make([]byte, 4096)
	count, err := response.Body.Read(buffer)
	if err != nil || !strings.HasPrefix(string(buffer[:count]), `data: [{"name":"one"`) {
		t.Fatalf("first frame: %q %v", buffer[:count], err)
	}
}

func TestConsoleMetricsNeedNoSession(t *testing.T) {
	h := newHarness(t, nil)
	h.hub.skipped.Add(3)
	h.hub.tailLag["one"] = 42
	body := new(bytes.Buffer)
	body.ReadFrom(h.do(t, http.MethodGet, "/metrics", "", nil).Body)
	for _, expected := range []string{
		`rosetta_console_instance_online{rosetta_instance="one"} 0`,
		`rosetta_console_tail_lag_bytes{rosetta_instance="one"} 42`,
		`rosetta_console_lines_skipped_total 3`,
	} {
		if !strings.Contains(body.String(), expected+"\n") {
			t.Fatalf("missing %s in\n%s", expected, body)
		}
	}
}

func TestTailCountsSkippedLinesAndLag(t *testing.T) {
	store := testStore(t)
	path := filepath.Join(t.TempDir(), "reactive.jsonl")
	appendFile(t, path, "{bad}\n"+skipLine(t, "a", 1)+`{"partial`)
	if err := store.trackFile(t.Context(), tailFile{Path: path, Instance: "one", RunID: "run", Kind: kindReactive}); err != nil {
		t.Fatal(err)
	}
	var skipped atomic.Uint64
	tailer := &Tailer{store: store, instance: "one", skipped: &skipped}
	if err := tailer.Pass(t.Context(), "run"); err != nil {
		t.Fatal(err)
	}
	if skipped.Load() != 1 || tailer.lag != int64(len(`{"partial`)) {
		t.Fatalf("skipped=%d lag=%d", skipped.Load(), tailer.lag)
	}
}

func TestStaticRoutesFallBackToIndex(t *testing.T) {
	h := newHarness(t, nil)
	for path, want := range map[string]string{"/": "console", "/sends": "console", "/assets/app.js": "app"} {
		response := h.do(t, http.MethodGet, path, "", nil)
		body := new(bytes.Buffer)
		body.ReadFrom(response.Body)
		if !strings.Contains(body.String(), want) {
			t.Fatalf("%s served %q", path, body)
		}
	}
	if response := h.do(t, http.MethodGet, "/api/unknown", "", nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown API path: %d", response.StatusCode)
	}
	if policy := h.do(t, http.MethodGet, "/", "", nil).Header.Get("Content-Security-Policy"); !strings.Contains(policy, "default-src 'self'") {
		t.Fatalf("CSP: %q", policy)
	}
}

func fakePrometheus(t *testing.T, handler http.HandlerFunc) *Prometheus {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewPrometheus(server.URL)
}

func TestChartAlignsSamplesAndKeepsGaps(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	var query string
	prometheus := fakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		query = r.URL.Query().Get("query")
		start := r.URL.Query().Get("start")
		step := r.URL.Query().Get("step")
		if step != "15" {
			t.Errorf("step %s", step)
		}
		w.Write([]byte(`{"status":"success","data":{"result":[
			{"metric":{"source":"standard"},"values":[[` + start + `,"1.5"],[` + start + `.0,"NaN"]]},
			{"metric":{"source":"fast"},"values":[]}]}}`))
	})
	history, err := prometheus.Chart(t.Context(), "one", "messages", "1h", now)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(query, `rosetta_instance="one"`) {
		t.Fatalf("query %s", query)
	}
	if len(history.Timestamps) != chartPoints+1 || len(history.Series) != 2 || history.Series[0].Name != "fast" || history.Series[1].Name != "standard" {
		t.Fatalf("history: %d points %+v", len(history.Timestamps), history.Series)
	}
	if values := history.Series[1].Values; values[1] != nil {
		t.Fatalf("gap filled: %v", values[1])
	}
}

func TestChartRejectsUnknownNames(t *testing.T) {
	prometheus := fakePrometheus(t, func(http.ResponseWriter, *http.Request) { t.Error("unknown chart reached prometheus") })
	for _, test := range [][2]string{{"up", "1h"}, {"messages", "99y"}} {
		if _, err := prometheus.Chart(t.Context(), "one", test[0], test[1], time.Now()); err != errUnknownChart {
			t.Fatalf("%v: %v", test, err)
		}
	}
}

func TestHistoryAndAlertsThroughAPI(t *testing.T) {
	h := newHarness(t, fakePrometheus(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/alerts" {
			w.Write([]byte(`{"status":"success","data":{"alerts":[{"labels":{"alertname":"RosettaDown","severity":"critical","rosetta_instance":"one"},"annotations":{"summary":"down"},"state":"firing","activeAt":"2026-11-01T00:00:00Z"}]}}`))
			return
		}
		w.Write([]byte(`{"status":"error","error":"boom"}`))
	}))
	h.signIn(t)
	var alerts []Alert
	if err := json.NewDecoder(h.do(t, http.MethodGet, "/api/alerts", "", nil).Body).Decode(&alerts); err != nil {
		t.Fatal(err)
	}
	if len(alerts) != 1 || alerts[0].Name != "RosettaDown" || alerts[0].Instance != "one" || alerts[0].State != "firing" {
		t.Fatalf("alerts: %+v", alerts)
	}
	if response := h.do(t, http.MethodGet, "/api/instances/one/history/messages?range=1h", "", nil); response.StatusCode != http.StatusBadGateway {
		t.Fatalf("prometheus failure: %d", response.StatusCode)
	}
	if response := h.do(t, http.MethodGet, "/api/instances/nope/history/messages?range=1h", "", nil); response.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown instance: %d", response.StatusCode)
	}
	if response := h.do(t, http.MethodGet, "/api/instances/one/history/up?range=1h", "", nil); response.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown chart: %d", response.StatusCode)
	}
}

func TestHistoryUnavailableWithoutPrometheus(t *testing.T) {
	h := newHarness(t, nil)
	h.signIn(t)
	if response := h.do(t, http.MethodGet, "/api/instances/one/history/messages?range=1h", "", nil); response.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("history: %d", response.StatusCode)
	}
}

// A run reports its output paths before the bot creates the files. A missing
// file of the current run is not yet written, not removed.
func TestTailWaitsForCurrentRunFileToAppear(t *testing.T) {
	store := testStore(t)
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "reactive.jsonl")
	if err := store.trackFile(ctx, tailFile{Path: path, Instance: "one", RunID: "run-1", Kind: kindReactive}); err != nil {
		t.Fatal(err)
	}
	tailer := &Tailer{store: store, instance: "one"}
	if err := tailer.Pass(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	appendFile(t, path, skipLine(t, "a", 1))
	if err := tailer.Pass(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	if counts := skipsStored(t, store); counts["a"] != 1 {
		t.Fatalf("late file of the current run not imported: %+v", counts)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := tailer.Pass(ctx, "run-2"); err != nil {
		t.Fatal(err)
	}
	if files, _ := store.openFiles(ctx, "one"); len(files) != 0 {
		t.Fatal("removed file of a retired run stayed open")
	}
}
