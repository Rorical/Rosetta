package telemetry

import (
	"fmt"
	"net/http"
	"strconv"

	"github.com/Rorical/Rosetta/internal/rpc"
)

type Bucket struct {
	UpperNS uint64 `json:"upper_ns"`
	Count   uint64 `json:"count"`
}

type Latency struct {
	Count   uint64   `json:"count"`
	SumNS   uint64   `json:"sum_ns"`
	Buckets []Bucket `json:"buckets"`
}

type SourceSnapshot struct {
	Counters map[string]uint64  `json:"counters"`
	Stages   map[string]Latency `json:"stages"`
	Gauges   map[string]int64   `json:"gauges"`
	// Estimates are simulated plan totals, not realized profit.
	Estimates map[string]uint64 `json:"estimates"`
}

type Snapshot struct {
	SchemaVersion uint32                    `json:"schema_version"`
	Sources       map[string]SourceSnapshot `json:"sources"`
	Process       Process                   `json:"process"`
	// RPCCallers attributes shared-policy RPC traffic to the component that issued it.
	RPCCallers map[string]RPCUsage `json:"rpc_callers"`
}

// RPCUsage counts HTTP attempts and method calls, one per batch member.
type RPCUsage struct {
	Requests uint64 `json:"requests"`
	Calls    uint64 `json:"calls"`
}

var sourceNames = [sourceCount]string{"standard", "fast"}

func (r *Registry) Snapshot() Snapshot {
	snapshot := Snapshot{SchemaVersion: 1, Sources: make(map[string]SourceSnapshot, sourceCount)}
	if r == nil {
		return snapshot
	}
	r.refreshSamples()
	snapshot.Process = readProcess()
	snapshot.RPCCallers = make(map[string]RPCUsage, rpc.CallerCount)
	for caller := range rpc.CallerCount {
		snapshot.RPCCallers[caller.String()] = RPCUsage{Requests: r.rpcRequests[caller].Load(), Calls: r.rpcCalls[caller].Load()}
	}
	for source, name := range sourceNames {
		values := SourceSnapshot{Counters: make(map[string]uint64, counterCount), Stages: make(map[string]Latency, stageCount), Gauges: make(map[string]int64, gaugeCount), Estimates: make(map[string]uint64, estimateCount)}
		for counter, name := range counterNames {
			values.Counters[name] = r.counters[source][counter].Load()
		}
		for gauge, name := range gaugeNames {
			values.Gauges[name] = r.gauges[source][gauge].Load()
		}
		for estimate, name := range estimateNames {
			values.Estimates[name] = r.estimates[source][estimate].Load()
		}
		for stage, name := range stageNames {
			h := &r.stages[source][stage]
			latency := Latency{SumNS: h.sum.Load(), Buckets: make([]Bucket, len(bounds))}
			for i := range h.bins {
				latency.Count += h.bins[i].Load()
				if i < len(bounds) {
					latency.Buckets[i] = Bucket{UpperNS: uint64(bounds[i]), Count: latency.Count}
				}
			}
			values.Stages[name] = latency
		}
		snapshot.Sources[name] = values
	}
	return snapshot
}

// ServeHTTP exposes liveness at /healthz and Prometheus text at /metrics with a
// fixed source/event/stage/gauge label set. The caller controls the listening
// address and endpoint lifecycle.
func (r *Registry) ServeHTTP(w http.ResponseWriter, request *http.Request) {
	if request.URL.Path != "/metrics" && request.URL.Path != "/healthz" {
		http.NotFound(w, request)
		return
	}
	if request.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path == "/healthz" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
		return
	}
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	snapshot := r.Snapshot()
	r.writeIdentity(w)
	writeProcess(w, snapshot.Process)
	fmt.Fprintln(w, "# HELP rosetta_pipeline_state Current pipeline position and connection state.")
	fmt.Fprintln(w, "# TYPE rosetta_pipeline_state gauge")
	for _, source := range sourceNames {
		for _, gauge := range gaugeNames {
			fmt.Fprintf(w, "rosetta_pipeline_state{source=%q,gauge=%q} %d\n", source, gauge, snapshot.Sources[source].Gauges[gauge])
		}
	}
	fmt.Fprintln(w, "# HELP rosetta_plan_estimate_total Simulated totals of ready plans, counted once per route run. Not realized profit.")
	fmt.Fprintln(w, "# TYPE rosetta_plan_estimate_total counter")
	for _, source := range sourceNames {
		for _, estimate := range estimateNames {
			fmt.Fprintf(w, "rosetta_plan_estimate_total{source=%q,item=%q} %d\n", source, estimate, snapshot.Sources[source].Estimates[estimate])
		}
	}
	fmt.Fprintln(w, "# HELP rosetta_pipeline_events_total Pipeline events by source and event.")
	fmt.Fprintln(w, "# TYPE rosetta_pipeline_events_total counter")
	for _, source := range sourceNames {
		for _, event := range counterNames {
			fmt.Fprintf(w, "rosetta_pipeline_events_total{source=%q,event=%q} %d\n", source, event, snapshot.Sources[source].Counters[event])
		}
	}
	fmt.Fprintln(w, "# HELP rosetta_rpc_requests_total Shared-policy RPC HTTP attempts by calling component.")
	fmt.Fprintln(w, "# TYPE rosetta_rpc_requests_total counter")
	for caller := range rpc.CallerCount {
		fmt.Fprintf(w, "rosetta_rpc_requests_total{caller=%q} %d\n", caller, snapshot.RPCCallers[caller.String()].Requests)
	}
	fmt.Fprintln(w, "# HELP rosetta_rpc_method_calls_total Shared-policy RPC method calls, one per batch member, by calling component.")
	fmt.Fprintln(w, "# TYPE rosetta_rpc_method_calls_total counter")
	for caller := range rpc.CallerCount {
		fmt.Fprintf(w, "rosetta_rpc_method_calls_total{caller=%q} %d\n", caller, snapshot.RPCCallers[caller.String()].Calls)
	}
	fmt.Fprintln(w, "# HELP rosetta_pipeline_duration_seconds Pipeline stage duration in seconds.")
	fmt.Fprintln(w, "# TYPE rosetta_pipeline_duration_seconds histogram")
	for _, source := range sourceNames {
		for _, stage := range stageNames {
			latency := snapshot.Sources[source].Stages[stage]
			for _, bucket := range latency.Buckets {
				bound := strconv.FormatFloat(float64(bucket.UpperNS)/1e9, 'f', -1, 64)
				fmt.Fprintf(w, "rosetta_pipeline_duration_seconds_bucket{source=%q,stage=%q,le=%q} %d\n", source, stage, bound, bucket.Count)
			}
			fmt.Fprintf(w, "rosetta_pipeline_duration_seconds_bucket{source=%q,stage=%q,le=\"+Inf\"} %d\n", source, stage, latency.Count)
			fmt.Fprintf(w, "rosetta_pipeline_duration_seconds_sum{source=%q,stage=%q} %s\n", source, stage, strconv.FormatFloat(float64(latency.SumNS)/1e9, 'f', -1, 64))
			fmt.Fprintf(w, "rosetta_pipeline_duration_seconds_count{source=%q,stage=%q} %d\n", source, stage, latency.Count)
		}
	}
}
