package console

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

const (
	pollInterval = time.Second
	// Rates need few samples; latency needs enough observations to estimate p99.
	rateWindow    = 10
	latencyWindow = 60
	pruneEvery    = time.Minute
	keepRows      = 1_000_000
	keepSkips     = 30 * 24 * time.Hour
)

// Instance names one bot and the control directory it was started with.
type Instance struct {
	Name, Dir string
}

// StageView estimates quantiles from the last minute of histogram deltas.
type StageView struct {
	Count    uint64 `json:"count"`
	Lifetime uint64 `json:"lifetime"`
	P50NS    uint64 `json:"p50_ns"`
	P90NS    uint64 `json:"p90_ns"`
	P99NS    uint64 `json:"p99_ns"`
	// Overflow marks p99 above the top finite bucket: only a lower bound is known.
	Overflow bool `json:"overflow"`
}

type SourceView struct {
	Counters  map[string]uint64    `json:"counters"`
	Gauges    map[string]int64     `json:"gauges"`
	Estimates map[string]uint64    `json:"estimates"`
	Rates     map[string]float64   `json:"rates"`
	Stages    map[string]StageView `json:"stages"`
}

// InstanceView is one instance as the UI sees it, refreshed every second.
type InstanceView struct {
	Name       string                `json:"name"`
	Online     bool                  `json:"online"`
	Error      string                `json:"error,omitempty"`
	LastSeenMS int64                 `json:"last_seen_ms"`
	Status     *control.Status       `json:"status,omitempty"`
	Gate       control.GateState     `json:"gate"`
	Process    telemetry.Process     `json:"process"`
	CPUPercent float64               `json:"cpu_percent"`
	Sources    map[string]SourceView `json:"sources,omitempty"`
}

type sample struct {
	at       time.Time
	snapshot telemetry.Snapshot
}

// Hub polls each instance and fans the combined view out to subscribers.
type Hub struct {
	store     *Store
	instances []Instance
	clients   map[string]*opsClient

	mu          sync.RWMutex
	views       map[string]InstanceView
	subscribers map[chan []byte]struct{}
	tailLag     map[string]int64
	skipped     atomic.Uint64
}

func NewHub(store *Store, instances []Instance) *Hub {
	hub := &Hub{store: store, instances: instances, clients: make(map[string]*opsClient), views: make(map[string]InstanceView), subscribers: make(map[chan []byte]struct{}), tailLag: make(map[string]int64)}
	for _, instance := range instances {
		hub.clients[instance.Name] = newOpsClient(instance.Dir)
		hub.views[instance.Name] = InstanceView{Name: instance.Name, Error: "waiting for first poll"}
	}
	return hub
}

// Run blocks until ctx ends.
func (h *Hub) Run(ctx context.Context) {
	var workers sync.WaitGroup
	for _, instance := range h.instances {
		workers.Go(func() { h.poll(ctx, instance.Name) })
		workers.Go(func() { h.tail(ctx, instance.Name) })
	}
	workers.Go(func() {
		for tailBackoff(ctx, pruneEvery) {
			if err := h.store.prune(ctx, keepRows, time.Now().Add(-keepSkips).UnixMilli()); err != nil && ctx.Err() == nil {
				slog.Warn("console: prune", "error", err)
			}
		}
	})
	workers.Wait()
}

func (h *Hub) poll(ctx context.Context, name string) {
	client := h.clients[name]
	var history []sample
	runID := ""
	// tracked is the last status whose outputs reached the tailer.
	var tracked control.Status
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		view, snapshot, err := fetchView(ctx, client, name)
		now := time.Now()
		if err != nil {
			previous := h.view(name)
			previous.Online, previous.Error = false, err.Error()
			h.publish(previous)
		} else {
			if view.Status.RunID != runID {
				runID, history = view.Status.RunID, nil
			}
			// Outputs are announced after startup, once their files are open.
			if view.Status.RunID != tracked.RunID || view.Status.Outputs != tracked.Outputs {
				if err := h.trackOutputs(ctx, name, view.Status); err != nil {
					slog.Warn("console: track run outputs", "instance", name, "error", err)
				} else {
					tracked = *view.Status
				}
			}
			history = append(history, sample{now, snapshot})
			if len(history) > latencyWindow+1 {
				history = history[1:]
			}
			view.LastSeenMS = now.UnixMilli()
			applySamples(&view, history)
			h.publish(view)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func fetchView(ctx context.Context, client *opsClient, name string) (InstanceView, telemetry.Snapshot, error) {
	ctx, cancel := context.WithTimeout(ctx, pollInterval)
	defer cancel()
	view := InstanceView{Name: name, Online: true}
	var status control.Status
	var snapshot telemetry.Snapshot
	if err := client.get(ctx, "/status", &status); err != nil {
		return view, snapshot, err
	}
	if err := client.get(ctx, "/snapshot", &snapshot); err != nil {
		return view, snapshot, err
	}
	if err := client.get(ctx, "/gate", &view.Gate); err != nil {
		return view, snapshot, err
	}
	view.Status = &status
	view.Process = snapshot.Process
	return view, snapshot, nil
}

func (h *Hub) trackOutputs(ctx context.Context, name string, status *control.Status) error {
	for _, output := range []struct{ kind, path string }{
		{kindCandidate, status.Outputs.Candidates}, {kindReactive, status.Outputs.Reactive}, {kindCompetition, status.Outputs.Competition},
	} {
		if output.path == "" {
			continue
		}
		if err := h.store.trackFile(ctx, tailFile{Path: output.path, Instance: name, RunID: status.RunID, Kind: output.kind}); err != nil {
			return err
		}
	}
	return nil
}

func (h *Hub) tail(ctx context.Context, name string) {
	tailer := &Tailer{store: h.store, instance: name, skipped: &h.skipped}
	for tailBackoff(ctx, pollInterval) {
		current := ""
		if status := h.view(name).Status; status != nil {
			current = status.RunID
		}
		if err := tailer.Pass(ctx, current); err != nil && ctx.Err() == nil {
			slog.Warn("console: tail outputs", "instance", name, "error", err)
		}
		h.mu.Lock()
		h.tailLag[name] = tailer.lag
		h.mu.Unlock()
	}
}

// applySamples fills rates and quantiles. Bot restarts reset the history, so
// every delta below compares monotonic counters of a single run.
func applySamples(view *InstanceView, history []sample) {
	latest := history[len(history)-1]
	rateBase := history[max(0, len(history)-1-rateWindow)]
	latencyBase := history[0]
	elapsed := latest.at.Sub(rateBase.at).Seconds()
	view.Sources = make(map[string]SourceView, len(latest.snapshot.Sources))
	for name, source := range latest.snapshot.Sources {
		out := SourceView{Counters: source.Counters, Gauges: source.Gauges, Estimates: source.Estimates, Rates: make(map[string]float64, len(source.Counters)), Stages: make(map[string]StageView, len(source.Stages))}
		for event, value := range source.Counters {
			if elapsed > 0 {
				out.Rates[event] = float64(value-rateBase.snapshot.Sources[name].Counters[event]) / elapsed
			}
		}
		for stage, latency := range source.Stages {
			out.Stages[stage] = stageView(latency, latencyBase.snapshot.Sources[name].Stages[stage])
		}
		view.Sources[name] = out
	}
	if elapsed > 0 {
		view.CPUPercent = (latest.snapshot.Process.CPUSeconds - rateBase.snapshot.Process.CPUSeconds) / elapsed * 100
	}
}

func stageView(latest, base telemetry.Latency) StageView {
	view := StageView{Lifetime: latest.Count, Count: latest.Count - base.Count}
	if view.Count == 0 {
		return view
	}
	delta := make([]telemetry.Bucket, len(latest.Buckets))
	for i, bucket := range latest.Buckets {
		delta[i] = bucket
		if i < len(base.Buckets) {
			delta[i].Count -= base.Buckets[i].Count
		}
	}
	view.P50NS, _ = quantile(delta, view.Count, 0.50)
	view.P90NS, _ = quantile(delta, view.Count, 0.90)
	view.P99NS, view.Overflow = quantile(delta, view.Count, 0.99)
	return view
}

// quantile interpolates linearly inside a cumulative bucket. Above the last
// finite bound it returns that bound and reports overflow.
func quantile(buckets []telemetry.Bucket, total uint64, q float64) (uint64, bool) {
	target := q * float64(total)
	lowerBound, lowerCount := uint64(0), uint64(0)
	for _, bucket := range buckets {
		if float64(bucket.Count) >= target && bucket.Count > lowerCount {
			fraction := (target - float64(lowerCount)) / float64(bucket.Count-lowerCount)
			return lowerBound + uint64(fraction*float64(bucket.UpperNS-lowerBound)), false
		}
		lowerBound, lowerCount = bucket.UpperNS, bucket.Count
	}
	return lowerBound, true
}

func (h *Hub) view(name string) InstanceView {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.views[name]
}

// Views returns instances in configuration order.
func (h *Hub) Views() []InstanceView {
	h.mu.RLock()
	defer h.mu.RUnlock()
	views := make([]InstanceView, 0, len(h.instances))
	for _, instance := range h.instances {
		views = append(views, h.views[instance.Name])
	}
	return views
}

func (h *Hub) publish(view InstanceView) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.views[view.Name] = view
	views := make([]InstanceView, 0, len(h.instances))
	for _, instance := range h.instances {
		views = append(views, h.views[instance.Name])
	}
	encoded, err := json.Marshal(views)
	if err != nil {
		slog.Error("console: encode views", "error", err)
		return
	}
	for subscriber := range maps.Keys(h.subscribers) {
		// Latest value wins: a slow browser skips frames instead of queueing them.
		select {
		case <-subscriber:
		default:
		}
		subscriber <- encoded
	}
}

// Subscribe returns a channel of encoded views and its cancel function.
func (h *Hub) Subscribe() (<-chan []byte, func()) {
	channel := make(chan []byte, 1)
	h.mu.Lock()
	defer h.mu.Unlock()
	h.subscribers[channel] = struct{}{}
	return channel, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		delete(h.subscribers, channel)
	}
}

// WriteMetrics reports the console's own health in Prometheus text.
func (h *Hub) WriteMetrics(w io.Writer) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	fmt.Fprintln(w, "# HELP rosetta_console_instance_online Whether the instance answered the last poll.")
	fmt.Fprintln(w, "# TYPE rosetta_console_instance_online gauge")
	for _, instance := range h.instances {
		online := 0
		if h.views[instance.Name].Online {
			online = 1
		}
		fmt.Fprintf(w, "rosetta_console_instance_online{rosetta_instance=%q} %d\n", instance.Name, online)
	}
	fmt.Fprintln(w, "# HELP rosetta_console_tail_lag_bytes Output bytes not yet imported.")
	fmt.Fprintln(w, "# TYPE rosetta_console_tail_lag_bytes gauge")
	for _, instance := range h.instances {
		fmt.Fprintf(w, "rosetta_console_tail_lag_bytes{rosetta_instance=%q} %d\n", instance.Name, h.tailLag[instance.Name])
	}
	fmt.Fprintln(w, "# HELP rosetta_console_lines_skipped_total Output lines that could not be decoded.")
	fmt.Fprintln(w, "# TYPE rosetta_console_lines_skipped_total counter")
	fmt.Fprintf(w, "rosetta_console_lines_skipped_total %d\n", h.skipped.Load())
}

func (h *Hub) Known(name string) bool {
	return slices.ContainsFunc(h.instances, func(i Instance) bool { return i.Name == name })
}

// opsClient speaks to one bot's control socket.
type opsClient struct {
	http *http.Client
}

func newOpsClient(dir string) *opsClient {
	socket := filepath.Join(dir, "ops.sock")
	return &opsClient{http: &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", socket)
		},
	}}}
}

func (c *opsClient) get(ctx context.Context, path string, target any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://rosetta"+path, nil)
	if err != nil {
		return err
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("control socket unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("control %s: status %d", path, response.StatusCode)
	}
	return json.NewDecoder(response.Body).Decode(target)
}

func (c *opsClient) setGate(ctx context.Context, paused bool, actor, reason string) (control.GateState, error) {
	body, err := json.Marshal(map[string]any{"paused": paused, "actor": actor, "reason": reason})
	if err != nil {
		return control.GateState{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://rosetta/gate", strings.NewReader(string(body)))
	if err != nil {
		return control.GateState{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(request)
	if err != nil {
		return control.GateState{}, fmt.Errorf("control socket unreachable")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return control.GateState{}, fmt.Errorf("gate change refused: status %d", response.StatusCode)
	}
	var state control.GateState
	return state, json.NewDecoder(response.Body).Decode(&state)
}
