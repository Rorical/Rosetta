package pipeline

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestPrefixGapRetainsExactLossBoundsWithoutCensoringQuotes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "paper.jsonl")
	research, err := NewResearch(t.Context(), "", path, domain.ChainIdentity{ChainID: 10})
	if err != nil {
		t.Fatal(err)
	}
	research.prefixGaps.add(40)
	research.prefixGaps.add(10)
	research.prefixGaps.add(30)
	if err := research.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	seen := false
	for decoder.More() {
		var row struct {
			Type    string `json:"type"`
			Skipped uint64 `json:"skipped_observations"`
			Details struct {
				First int64 `json:"first_received_at_ns"`
				Last  int64 `json:"last_received_at_ns"`
			} `json:"details"`
		}
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		if row.Type == "gap" {
			t.Fatal("prefix tracing loss censored quote observations")
		}
		if row.Type == "prefix_gap" {
			seen = true
			if row.Skipped != 3 || row.Details.First != 10 || row.Details.Last != 40 {
				t.Fatal("lost gap extent", row)
			}
		}
	}
	if !seen {
		t.Fatal("missing prefix gap")
	}
}
