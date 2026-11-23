package telemetry

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/rpc"
)

func TestConcurrentMetricsAndCumulativeBuckets(t *testing.T) {
	registry := &Registry{}
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for range 1000 {
				registry.Add(Fast, Messages, 1)
				registry.Observe(Fast, PoolApply, time.Millisecond)
			}
		}()
	}
	for range 20 {
		registry.Snapshot()
	}
	workers.Wait()
	registry.Observe(Fast, PoolApply, 6*time.Second)
	registry.Observe(Fast, PoolApply, -time.Second)
	registry.Observe(Source(255), PoolApply, time.Second)
	registry.Add(Fast, Counter(255), 1)
	snapshot := registry.Snapshot()
	got := snapshot.Sources["fast"]
	latency := got.Stages["pool_apply"]
	if got.Counters["messages"] != 8000 || latency.Count != 8001 || latency.SumNS != uint64(8*time.Second+6*time.Second) {
		t.Fatalf("lost concurrent observations: %+v", got)
	}
	if latency.Buckets[1].Count != 0 || latency.Buckets[2].Count != 8000 || latency.Buckets[len(bounds)-1].Count != 8000 {
		t.Fatalf("bucket boundaries or infinity sample wrong: %+v", latency)
	}
	if snapshot.Sources["standard"].Counters["messages"] != 0 || len(snapshot.Sources) != 2 {
		t.Fatal("source labels mixed or unbounded")
	}
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	var decoded Snapshot
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded.Sources["fast"].Stages["pool_apply"].Count != 8001 {
		t.Fatalf("snapshot roundtrip: %v", err)
	}
}

func TestPrometheusEndpointAndDisabledCollection(t *testing.T) {
	var disabled *Registry
	if !disabled.Start().IsZero() {
		t.Fatal("disabled collection read the clock")
	}
	disabled.Since(Fast, PoolApply, time.Time{})
	disabled.Add(Fast, Messages, 1)
	disabled.Observe(Fast, FeedQueue, time.Second)
	registry := &Registry{}
	registry.Add(Standard, ReadyPlans, 2)
	registry.Observe(Standard, ReadyLatency, 250*time.Microsecond)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, expected := range []string{
		`rosetta_pipeline_events_total{source="standard",event="ready_plans"} 2`,
		`rosetta_pipeline_duration_seconds_bucket{source="standard",stage="ready_latency",le="0.00025"} 1`,
		`rosetta_pipeline_duration_seconds_bucket{source="standard",stage="ready_latency",le="+Inf"} 1`,
		`rosetta_pipeline_duration_seconds_sum{source="standard",stage="ready_latency"} 0.00025`,
	} {
		if !strings.Contains(response.Body.String(), expected+"\n") {
			t.Fatalf("missing %s", expected)
		}
	}
	for _, request := range []struct {
		method, path string
		status       int
	}{
		{http.MethodGet, "/other", http.StatusNotFound},
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodPost, "/healthz", http.StatusMethodNotAllowed},
		{http.MethodPost, "/metrics", http.StatusMethodNotAllowed},
	} {
		response := httptest.NewRecorder()
		registry.ServeHTTP(response, httptest.NewRequest(request.method, request.path, nil))
		if response.Code != request.status {
			t.Fatalf("%s %s: %d", request.method, request.path, response.Code)
		}
	}
}

func TestGaugesProcessAndIdentityExport(t *testing.T) {
	var disabled *Registry
	disabled.Set(Fast, HeadBlock, 1)
	disabled.Sample(Fast, FeedBacklog, func() int64 { return 1 })
	if disabled.ConnectionObserver(Fast) != nil {
		t.Fatal("disabled registry returned a connection observer")
	}
	registry := &Registry{build: Build{Version: "abc", Backend: "rpc", Network: "arbitrum-one", ChainID: 42161}, started: time.Unix(1700000000, 0)}
	registry.Head(Standard, 42, 1800000000)
	registry.Sample(Fast, FeedBacklog, func() int64 { return 7 })
	registry.ConnectionObserver(Standard)(true)
	registry.Set(Source(255), HeadBlock, 1)
	registry.Set(Standard, Gauge(255), 1)
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, expected := range []string{
		`rosetta_build_info{version="abc",backend="rpc",network="arbitrum-one",chain_id="42161"} 1`,
		`rosetta_start_time_seconds 1700000000`,
		`rosetta_pipeline_state{source="standard",gauge="head_block"} 42`,
		`rosetta_pipeline_state{source="standard",gauge="head_timestamp_seconds"} 1800000000`,
		`rosetta_pipeline_state{source="standard",gauge="feed_connected"} 1`,
		`rosetta_pipeline_state{source="fast",gauge="feed_backlog"} 7`,
	} {
		if !strings.Contains(response.Body.String(), expected+"\n") {
			t.Fatalf("missing %s", expected)
		}
	}
	if !strings.Contains(response.Body.String(), "rosetta_process_goroutines ") {
		t.Fatal("missing process metrics")
	}
	snapshot := registry.Snapshot()
	if snapshot.Process.Goroutines == 0 || snapshot.Process.MemoryBytes == 0 || snapshot.Process.CPUSeconds <= 0 || snapshot.Process.OpenFiles == 0 {
		t.Fatalf("process snapshot incomplete: %+v", snapshot.Process)
	}
}

func TestRPCResponsesFoldIntoFixedStages(t *testing.T) {
	var disabled *Registry
	disabled.ObserveRPCResponse(rpc.ResponseObservation{Method: "eth_call", Elapsed: time.Second})
	registry := &Registry{}
	for _, response := range []rpc.ResponseObservation{
		{Method: "eth_getBlockByHash", Elapsed: time.Millisecond},
		{Method: "eth_getBlockReceipts", Elapsed: time.Millisecond},
		{Method: "eth_call", Elapsed: time.Millisecond, Failed: true},
		{Method: rpc.BatchMethod, Elapsed: time.Millisecond, Failed: true, Throttled: true},
		{Method: "provider_invented", Elapsed: time.Millisecond},
		{Elapsed: time.Millisecond},
	} {
		registry.ObserveRPCResponse(response)
	}
	snapshot := registry.Snapshot().Sources["standard"]
	for stage, want := range map[string]uint64{"rpc_block": 1, "rpc_receipts": 1, "rpc_call": 1, "rpc_batch": 1, "rpc_other": 2, "rpc_logs": 0} {
		if snapshot.Stages[stage].Count != want {
			t.Errorf("%s = %d, want %d", stage, snapshot.Stages[stage].Count, want)
		}
	}
	if snapshot.Counters["rpc_http_errors"] != 2 || snapshot.Counters["rpc_throttled"] != 1 {
		t.Fatalf("errors: %+v", snapshot.Counters)
	}
}

// Callers tag their context; the shared policy books every HTTP attempt and
// batch member under that tag, and untagged traffic stays visible as "other".
func TestRPCRequestsAreAttributedToTheirCaller(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw json.RawMessage
		json.NewDecoder(r.Body).Decode(&raw)
		if strings.HasPrefix(string(raw), "[") {
			var calls []struct{ ID uint64 }
			json.Unmarshal(raw, &calls)
			responses := make([]map[string]any, len(calls))
			for i, call := range calls {
				responses[i] = map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0x1"}
			}
			json.NewEncoder(w).Encode(responses)
			return
		}
		var call struct{ ID uint64 }
		json.Unmarshal(raw, &call)
		json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": call.ID, "result": "0x1"})
	}))
	defer server.Close()
	registry := &Registry{}
	ctx := rpc.WithRequestPolicy(t.Context(), rpc.NewRequestPolicy(rpc.Options{Observe: registry.ObserveRPC}))
	client := rpc.NewClient(server.URL, rpc.Options{})
	follower := rpc.WithCaller(ctx, rpc.CallerFollower)
	if _, err := client.Batch(follower, []rpc.Call{{Method: "eth_call"}, {Method: "eth_call"}, {Method: "eth_getBalance"}}); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(follower, "eth_blockNumber", nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := client.Call(ctx, "eth_chainId", nil, nil); err != nil {
		t.Fatal(err)
	}
	callers := registry.Snapshot().RPCCallers
	if callers["follower"] != (RPCUsage{Requests: 2, Calls: 4}) || callers["other"] != (RPCUsage{Requests: 1, Calls: 1}) || callers["paper"] != (RPCUsage{}) {
		t.Fatalf("caller attribution: %+v", callers)
	}
	response := httptest.NewRecorder()
	registry.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	for _, expected := range []string{`rosetta_rpc_requests_total{caller="follower"} 2`, `rosetta_rpc_method_calls_total{caller="follower"} 4`, `rosetta_rpc_method_calls_total{caller="discovery"} 0`} {
		if !strings.Contains(response.Body.String(), expected+"\n") {
			t.Fatalf("missing %s", expected)
		}
	}
}

func TestDisconnectsAreCounted(t *testing.T) {
	registry := &Registry{}
	observe := registry.ConnectionObserver(Fast)
	observe(true)
	observe(false)
	observe(true)
	observe(false)
	fast := registry.Snapshot().Sources["fast"]
	if fast.Counters["feed_disconnects"] != 2 || fast.Gauges["feed_connected"] != 0 {
		t.Fatalf("connection metrics: %d %d", fast.Counters["feed_disconnects"], fast.Gauges["feed_connected"])
	}
}

func BenchmarkHotPathOperations(b *testing.B) {
	registry := &Registry{}
	var disabled *Registry
	b.Run("add", func(b *testing.B) {
		for b.Loop() {
			registry.Add(Standard, Logs, 1)
		}
	})
	b.Run("observe", func(b *testing.B) {
		for b.Loop() {
			registry.Observe(Standard, PoolApply, time.Millisecond)
		}
	})
	b.Run("start_since", func(b *testing.B) {
		for b.Loop() {
			registry.Since(Standard, PoolApply, registry.Start())
		}
	})
	b.Run("start_since_disabled", func(b *testing.B) {
		for b.Loop() {
			disabled.Since(Standard, PoolApply, disabled.Start())
		}
	})
	b.Run("head", func(b *testing.B) {
		for b.Loop() {
			registry.Head(Standard, 1, 1)
		}
	})
	b.Run("snapshot", func(b *testing.B) {
		for b.Loop() {
			registry.Snapshot()
		}
	})
}
