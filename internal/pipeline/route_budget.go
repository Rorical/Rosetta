package pipeline

import (
	"fmt"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
)

// A route costs about 70–104 bytes once packed and 113 more while its table
// is built, for four hops on a graph of about 750 pools. A
// rebuild holds the old and new tables together, so the budget assumes 64
// bytes per hop plus 64 per route, 320 at four hops, and gives the route
// table a quarter of the memory limit; pool state, tick windows, the explorer
// cache and research keep the rest.
const (
	routeBytesPerHop = 64
	routeBytesFixed  = 64
	routeMemoryShare = 4
)

// routesPerCore bounds the table by search CPU: a frame ranks every route
// through its dirty pools, so its cost grows with the table. A complete
// 8.9M-route table on 7 search cores, 1.28M a core, preempts no frame,
// finishes 99% of frames within about 90 ms and quotes two-hop routes in about
// 13 ms (p50) in the full search; the fast lane quotes short routes
// independently of the table. The margin above that point leaves room for
// discovery to add pools.
const routesPerCore = 1_500_000

// maxRouteBudget keeps a table's hops within the packed layout's uint32
// offsets at sixteen hops a route.
const maxRouteBudget = math.MaxUint32 / 16

// AutoMaxCycles sizes the route table for a memory limit and search cores at a
// search depth; the smaller bound wins.
func AutoMaxCycles(limit uint64, cores, depth int) int {
	perRoute := uint64(routeBytesFixed + routeBytesPerHop*depth)
	memory := limit / routeMemoryShare / perRoute
	cpu := uint64(max(1, cores)) * routesPerCore
	return int(min(memory, cpu, maxRouteBudget))
}

// MemoryLimit is the memory this process may use: GOMEMLIMIT when set, else
// its cgroup's memory.max, else physical memory. Source names which.
func MemoryLimit() (limit uint64, source string, err error) {
	if set := debug.SetMemoryLimit(-1); set != math.MaxInt64 {
		return uint64(set), "GOMEMLIMIT", nil
	}
	if membership, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		if dir, ok := cgroupDir(string(membership)); ok {
			if value, err := os.ReadFile("/sys/fs/cgroup" + dir + "/memory.max"); err == nil {
				if limit, ok := parseMemoryMax(string(value)); ok {
					return limit, "cgroup memory.max", nil
				}
			}
		}
	}
	meminfo, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, "", fmt.Errorf("memory limit: no GOMEMLIMIT, cgroup limit or /proc/meminfo: %w", err)
	}
	limit, err = parseMemTotal(string(meminfo))
	return limit, "physical memory", err
}

// cgroupDir is the unified (v2) hierarchy path in /proc/self/cgroup.
func cgroupDir(membership string) (string, bool) {
	for line := range strings.Lines(membership) {
		if path, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return path, true
		}
	}
	return "", false
}

// parseMemoryMax reads memory.max; "max" is no limit.
func parseMemoryMax(value string) (uint64, bool) {
	limit, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
	return limit, err == nil && limit > 0
}

func parseMemTotal(meminfo string) (uint64, error) {
	for line := range strings.Lines(meminfo) {
		fields := strings.Fields(line)
		if len(fields) == 3 && fields[0] == "MemTotal:" && fields[2] == "kB" {
			kb, err := strconv.ParseUint(fields[1], 10, 64)
			if err != nil {
				return 0, fmt.Errorf("parse MemTotal: %w", err)
			}
			return kb * 1024, nil
		}
	}
	return 0, fmt.Errorf("no MemTotal in /proc/meminfo")
}
