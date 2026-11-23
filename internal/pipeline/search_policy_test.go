package pipeline

import (
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestSearchPolicyMinGrossIsOptionalAndPositive(t *testing.T) {
	load := func(minGross string) (SearchPolicy, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "search.json")
		document := `{"k":4,"max_depth":3,"max_cycles":10,"funding":[{"token":"0x4200000000000000000000000000000000000006","min_input":"1","seed_input":"10","max_input":"100"` + minGross + `}]}`
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadSearchPolicy(path)
	}
	weth, _ := domain.ParseAddress("0x4200000000000000000000000000000000000006")
	open, err := load("")
	if err != nil || open.Funding[weth].Bounds.Floor != nil {
		t.Fatal("absent min_gross must leave ranking unfloored", err)
	}
	floored, err := load(`,"min_gross":"10000000000"`)
	if err != nil || floored.Funding[weth].Bounds.Floor.String() != "10000000000" {
		t.Fatal("min_gross not loaded", err)
	}
	if _, err := load(`,"min_gross":"0"`); err == nil {
		t.Fatal("zero min_gross accepted")
	}
}

func TestSearchPolicyExactKDefaultsToK(t *testing.T) {
	load := func(exactK string) (SearchPolicy, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "search.json")
		document := `{"k":4` + exactK + `,"max_depth":3,"max_cycles":10,"funding":[{"token":"0x4200000000000000000000000000000000000006","min_input":"1","seed_input":"10","max_input":"100"}]}`
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadSearchPolicy(path)
	}
	if policy, err := load(""); err != nil || policy.ExactK != 4 {
		t.Fatal("omitted exact_k must size all k candidates", policy.ExactK, err)
	}
	if policy, err := load(`,"exact_k":2`); err != nil || policy.ExactK != 2 {
		t.Fatal("exact_k not loaded", policy.ExactK, err)
	}
	for _, invalid := range []string{`,"exact_k":0`, `,"exact_k":5`, `,"exact_k":-1`} {
		if _, err := load(invalid); err == nil {
			t.Fatal("invalid exact_k accepted", invalid)
		}
	}
}

func TestSearchPolicyExplorePaths(t *testing.T) {
	load := func(explorePaths string) (SearchPolicy, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "search.json")
		document := `{"k":4,"max_depth":3,"max_cycles":10` + explorePaths + `,"funding":[{"token":"0x4200000000000000000000000000000000000006","min_input":"1","seed_input":"10","max_input":"100"}]}`
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadSearchPolicy(path)
	}
	if policy, err := load(""); err != nil || policy.ExplorePaths != 8192 {
		t.Fatalf("omitted explore_paths = %d, %v; want 8192", policy.ExplorePaths, err)
	}
	if policy, err := load(`,"explore_paths":0`); err != nil || policy.ExplorePaths != 8192 {
		t.Fatalf("zero explore_paths = %d, %v; want 8192", policy.ExplorePaths, err)
	}
	for _, valid := range []struct {
		value string
		want  int
	}{{`,"explore_paths":1024`, 1024}, {`,"explore_paths":32768`, 32768}, {`,"explore_paths":262144`, 262144}} {
		if policy, err := load(valid.value); err != nil || policy.ExplorePaths != valid.want {
			t.Fatalf("%s = %d, %v; want %d", valid.value, policy.ExplorePaths, err, valid.want)
		}
	}
	for _, invalid := range []string{`,"explore_paths":1023`, `,"explore_paths":262145`, `,"explore_paths":-1`} {
		_, err := load(invalid)
		if err == nil {
			t.Fatal("invalid explore_paths accepted", invalid)
		}
		if !strings.Contains(err.Error(), "explore_paths") {
			t.Fatalf("%s error does not name the field: %v", invalid, err)
		}
	}
}

func TestSearchPolicyFastLaneHopsWithinDepth(t *testing.T) {
	load := func(hops string) (SearchPolicy, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "search.json")
		document := `{"k":4,"max_depth":3,"max_cycles":10` + hops + `,"funding":[{"token":"0x4200000000000000000000000000000000000006","min_input":"1","seed_input":"10","max_input":"100"}]}`
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadSearchPolicy(path)
	}
	if policy, err := load(""); err != nil || policy.FastLaneHops != 0 {
		t.Fatal("omitted fast_lane_hops must keep the default", policy.FastLaneHops, err)
	}
	if policy, err := load(`,"fast_lane_hops":3`); err != nil || policy.FastLaneHops != 3 {
		t.Fatal("fast_lane_hops not loaded", policy.FastLaneHops, err)
	}
	for _, invalid := range []string{`,"fast_lane_hops":1`, `,"fast_lane_hops":4`, `,"fast_lane_hops":-2`} {
		if _, err := load(invalid); err == nil {
			t.Fatal("invalid fast_lane_hops accepted", invalid)
		}
	}
}

func TestSearchPolicyMaxCyclesFixedOrAuto(t *testing.T) {
	load := func(maxCycles string) (SearchPolicy, error) {
		t.Helper()
		path := filepath.Join(t.TempDir(), "search.json")
		document := `{"k":4,"max_depth":4` + maxCycles + `,"funding":[{"token":"0x4200000000000000000000000000000000000006","min_input":"1","seed_input":"10","max_input":"100"}]}`
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		return LoadSearchPolicy(path)
	}
	if policy, err := load(`,"max_cycles":500000`); err != nil || policy.MaxCycles != 500000 || policy.MaxCyclesSource != "" {
		t.Fatal("fixed max_cycles", policy.MaxCycles, policy.MaxCyclesSource, err)
	}
	previous := debug.SetMemoryLimit(14 << 30)
	defer debug.SetMemoryLimit(previous)
	policy, err := load(`,"max_cycles":"auto"`)
	if err != nil || policy.MaxCycles != AutoMaxCycles(14<<30, runtime.GOMAXPROCS(0), 4) || !strings.HasPrefix(policy.MaxCyclesSource, "auto: GOMEMLIMIT") {
		t.Fatal("auto max_cycles", policy.MaxCycles, policy.MaxCyclesSource, err)
	}
	for _, invalid := range []string{``, `,"max_cycles":0`, `,"max_cycles":-1`, `,"max_cycles":"many"`, `,"max_cycles":1e12`} {
		if _, err := load(invalid); err == nil || !strings.Contains(err.Error(), "max_cycles") {
			t.Fatalf("%q accepted or unnamed: %v", invalid, err)
		}
	}
}
