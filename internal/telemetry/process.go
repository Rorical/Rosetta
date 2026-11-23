package telemetry

import (
	"fmt"
	"io"
	"os"
	"runtime/metrics"
	"strconv"
	"syscall"
)

// Build identifies the running binary and chain for dashboards and alerts.
type Build struct {
	Version string
	Backend string
	Network string
	ChainID uint64
}

// Process holds Go runtime and OS totals. Reads go through runtime/metrics,
// which avoids the stop-the-world pause of runtime.ReadMemStats.
type Process struct {
	Goroutines   uint64  `json:"goroutines"`
	HeapBytes    uint64  `json:"heap_bytes"`
	MemoryBytes  uint64  `json:"memory_bytes"`
	GCCycles     uint64  `json:"gc_cycles"`
	GCCPUSeconds float64 `json:"gc_cpu_seconds"`
	// GCPauseSeconds is CPU time spent with the world stopped, summed over Ps.
	GCPauseSeconds float64 `json:"gc_pause_seconds"`
	CPUSeconds     float64 `json:"cpu_seconds"`
	OpenFiles      uint64  `json:"open_files"`
}

func readProcess() Process {
	samples := []metrics.Sample{
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/total:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/cpu/classes/gc/total:cpu-seconds"},
		{Name: "/cpu/classes/gc/pause:cpu-seconds"},
	}
	metrics.Read(samples)
	process := Process{
		Goroutines:     samples[0].Value.Uint64(),
		HeapBytes:      samples[1].Value.Uint64(),
		MemoryBytes:    samples[2].Value.Uint64(),
		GCCycles:       samples[3].Value.Uint64(),
		GCCPUSeconds:   samples[4].Value.Float64(),
		GCPauseSeconds: samples[5].Value.Float64(),
	}
	process.OpenFiles = openFiles()
	var usage syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &usage) == nil {
		process.CPUSeconds = float64(usage.Utime.Nano()+usage.Stime.Nano()) / 1e9
	}
	return process
}

// openFiles lists the descriptor table both Linux and Darwin expose. Names only:
// os.ReadDir stats each entry, which fails on Darwin for the listing's own fd.
func openFiles() uint64 {
	table, err := os.Open("/dev/fd")
	if err != nil {
		return 0
	}
	defer table.Close()
	names, err := table.Readdirnames(-1)
	if err != nil {
		return 0
	}
	return uint64(len(names))
}

func (r *Registry) writeIdentity(w io.Writer) {
	if r == nil || r.started.IsZero() {
		return
	}
	fmt.Fprintln(w, "# HELP rosetta_build_info Running binary and selected chain.")
	fmt.Fprintln(w, "# TYPE rosetta_build_info gauge")
	fmt.Fprintf(w, "rosetta_build_info{version=%q,backend=%q,network=%q,chain_id=\"%d\"} 1\n", r.build.Version, r.build.Backend, r.build.Network, r.build.ChainID)
	fmt.Fprintln(w, "# HELP rosetta_start_time_seconds Process start in Unix seconds.")
	fmt.Fprintln(w, "# TYPE rosetta_start_time_seconds gauge")
	fmt.Fprintf(w, "rosetta_start_time_seconds %d\n", r.started.Unix())
}

func writeProcess(w io.Writer, process Process) {
	seconds := func(value float64) string { return strconv.FormatFloat(value, 'f', -1, 64) }
	for _, metric := range []struct{ name, kind, help, value string }{
		{"rosetta_process_goroutines", "gauge", "Live goroutines.", strconv.FormatUint(process.Goroutines, 10)},
		{"rosetta_process_heap_bytes", "gauge", "Heap memory occupied by live and unswept objects.", strconv.FormatUint(process.HeapBytes, 10)},
		{"rosetta_process_memory_bytes", "gauge", "Memory mapped by the Go runtime.", strconv.FormatUint(process.MemoryBytes, 10)},
		{"rosetta_process_gc_cycles_total", "counter", "Completed GC cycles.", strconv.FormatUint(process.GCCycles, 10)},
		{"rosetta_process_gc_cpu_seconds_total", "counter", "Estimated CPU spent in GC.", seconds(process.GCCPUSeconds)},
		{"rosetta_process_gc_pause_seconds_total", "counter", "CPU time spent with the world stopped for GC.", seconds(process.GCPauseSeconds)},
		{"rosetta_process_cpu_seconds_total", "counter", "User and system CPU time.", seconds(process.CPUSeconds)},
		{"rosetta_process_open_files", "gauge", "Open file descriptors.", strconv.FormatUint(process.OpenFiles, 10)},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s %s\n%s %s\n", metric.name, metric.help, metric.name, metric.kind, metric.name, metric.value)
	}
}
