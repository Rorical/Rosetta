package pipeline

import "testing"

// The smaller of the memory and CPU bounds wins: a 14 GiB host with 7 search
// cores gets the CPU bound and holds a complete 8.9M-route table.
func TestAutoMaxCyclesTakesTheSmallerBound(t *testing.T) {
	for _, c := range []struct {
		limit        uint64
		cores, depth int
		want         int
	}{{14 << 30, 7, 4, 10_500_000}, {14 << 30, 1000, 4, 11_744_051}, {512 << 20, 7, 4, 419_430}, {14 << 30, 0, 4, 1_500_000}, {1 << 50, 1 << 20, 16, maxRouteBudget}} {
		if got := AutoMaxCycles(c.limit, c.cores, c.depth); got != c.want {
			t.Errorf("AutoMaxCycles(%d, %d, %d) = %d, want %d", c.limit, c.cores, c.depth, got, c.want)
		}
	}
	if AutoMaxCycles(14<<30, 7, 4) < 8_942_802 {
		t.Fatal("the reference memory budget no longer holds the complete table")
	}
}

func TestMemoryLimitSources(t *testing.T) {
	if dir, ok := cgroupDir("12:pids:/x\n0::/system.slice/rosetta-arbitrum.service\n"); !ok || dir != "/system.slice/rosetta-arbitrum.service" {
		t.Fatal("cgroup v2 path", dir, ok)
	}
	if _, ok := cgroupDir("12:pids:/x\n"); ok {
		t.Fatal("a v1-only membership has no unified path")
	}
	if limit, ok := parseMemoryMax("15032385536\n"); !ok || limit != 14<<30 {
		t.Fatal("memory.max", limit, ok)
	}
	if _, ok := parseMemoryMax("max\n"); ok {
		t.Fatal("an unlimited cgroup has no limit")
	}
	if limit, err := parseMemTotal("MemFree: 1 kB\nMemTotal:       16070216 kB\n"); err != nil || limit != 16070216*1024 {
		t.Fatal("MemTotal", limit, err)
	}
	if _, err := parseMemTotal("MemFree: 1 kB\n"); err == nil {
		t.Fatal("meminfo without MemTotal accepted")
	}
}
