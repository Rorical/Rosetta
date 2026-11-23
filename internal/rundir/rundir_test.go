package rundir

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestCreateAndMarkFollowMaintenanceLayout(t *testing.T) {
	state := t.TempDir()
	root, err := Open(state, []Copy{{Name: "search.json", Data: []byte(`{"k":1}`)}})
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 2, 22, 54, 7, 0, time.FixedZone("CET", 3600))
	run, err := root.Create(at)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(state, "runs", "20261102T215407-"+strconv.Itoa(os.Getpid())); run.Dir != want {
		t.Fatalf("run directory %s, want %s", run.Dir, want)
	}
	if info, err := os.Stat(run.Dir); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("run directory mode: %v %v", info, err)
	}
	if copied, err := os.ReadFile(filepath.Join(run.Dir, "search.json")); err != nil || string(copied) != `{"k":1}` {
		t.Fatalf("configuration copy: %q %v", copied, err)
	}
	again, err := root.Create(at)
	if err != nil || again.Dir != run.Dir+"-2" {
		t.Fatalf("second run in the same second: %s %v", again.Dir, err)
	}
	if err := root.Mark(run); err != nil {
		t.Fatal(err)
	}
	if marker, err := os.ReadFile(filepath.Join(state, "current-run")); err != nil || string(marker) != run.Dir+"\n" {
		t.Fatalf("marker: %q %v", marker, err)
	}
	if run.Pools() != filepath.Join(run.Dir, "final-pools.json") || run.Candidates() != filepath.Join(run.Dir, "candidates.jsonl") {
		t.Fatal("run file names differ from the maintenance script's")
	}
}
