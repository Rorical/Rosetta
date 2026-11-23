package console

import (
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

func candidateLines(t *testing.T, count int) string {
	t.Helper()
	var output strings.Builder
	pools := []domain.PoolKey{{Address: domain.Address{0xaa}, Kind: domain.PoolKindV3}, {Address: domain.Address{0xbb}, Kind: domain.PoolKindV2}}
	opportunity := arb.Opportunity{Cycle: graph.Cycle{Pools: pools, Start: domain.Address{0x51}, Len: 2}, Input: big.NewInt(10), Output: big.NewInt(13), Net: big.NewInt(3), GasCost: new(big.Int), NetAfterGas: big.NewInt(3)}
	event := arb.SearchEvent{Job: arb.SearchJob{Version: domain.StateVersion{Source: domain.SourceFast, BlockNumber: 80}}}
	for range count {
		event.Result.Opportunities = append(event.Result.Opportunities, opportunity)
	}
	if err := (pipeline.CandidateWriter{Encoder: json.NewEncoder(&output)}).Consume(t.Context(), event); err != nil {
		t.Fatal(err)
	}
	return output.String()
}

// An in-process rotation replaces the run ID and candidate path while the
// reactive file carries on. The console reads the closed candidate file to its
// end, retires it, follows the new one, and keeps its reactive offset.
func TestConsoleFollowsRotatedCandidatePath(t *testing.T) {
	store := testStore(t)
	hub := &Hub{store: store}
	ctx := t.Context()
	dir := t.TempDir()
	reactive := filepath.Join(dir, "reactive.jsonl")
	runs := []string{filepath.Join(dir, "runs", "a"), filepath.Join(dir, "runs", "b")}
	for _, run := range runs {
		if err := os.MkdirAll(run, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	first, second := filepath.Join(runs[0], "candidates.jsonl"), filepath.Join(runs[1], "candidates.jsonl")
	tailer := &Tailer{store: store, instance: "one"}

	status := control.Status{RunID: "run-1", Outputs: control.Outputs{Candidates: first, Reactive: reactive}}
	if err := hub.trackOutputs(ctx, "one", &status); err != nil {
		t.Fatal(err)
	}
	appendFile(t, reactive, skipLine(t, "a", 1))
	appendFile(t, first, candidateLines(t, 2))
	if err := tailer.Pass(ctx, "run-1"); err != nil {
		t.Fatal(err)
	}
	// The closed run's last records can still be unread when the new status arrives.
	appendFile(t, first, candidateLines(t, 1))
	status = control.Status{RunID: "run-2", Outputs: control.Outputs{Candidates: second, Reactive: reactive}}
	if err := hub.trackOutputs(ctx, "one", &status); err != nil {
		t.Fatal(err)
	}
	appendFile(t, second, candidateLines(t, 3))
	appendFile(t, reactive, skipLine(t, "a", 2))
	if err := tailer.Pass(ctx, "run-2"); err != nil {
		t.Fatal(err)
	}

	candidates, err := store.Candidates(ctx, "one", "fast", "gross", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidates) != 6 {
		t.Fatalf("%d candidates imported across the rotation, want 6", len(candidates))
	}
	if counts := skipsStored(t, store); counts["a"] != 3 {
		t.Fatalf("reactive file lost or repeated lines across the rotation: %+v", counts)
	}
	files, err := store.openFiles(ctx, "one")
	if err != nil {
		t.Fatal(err)
	}
	open := map[string]string{}
	for _, file := range files {
		open[file.Path] = file.RunID
	}
	if len(open) != 2 || open[second] != "run-2" || open[reactive] != "run-2" {
		t.Fatalf("files still followed: %v", open)
	}
}
