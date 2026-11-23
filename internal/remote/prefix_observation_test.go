package remote

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/feed"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

func TestPaperPrefixRecordsEmptyFramesAndExactTransactionRanges(t *testing.T) {
	r, pools := flashChainRuntime(t)
	path := filepath.Join(t.TempDir(), "observations.jsonl")
	research, err := pipeline.NewResearch(t.Context(), "", path, domain.ChainIdentity{ChainID: 10})
	if err != nil {
		t.Fatal(err)
	}
	r.research = research
	received := time.Now()
	empty := chainFrame(pools[0], 11, 3000000)
	empty.Transactions = nil
	if err := r.acceptFlashblock(t.Context(), empty, received); err != nil {
		t.Fatal(err)
	}
	first := chainFrame(pools[0], 11, 3000000)
	first.Base = nil
	first.Index = 1
	if err := r.acceptFlashblock(t.Context(), first, received); err != nil {
		t.Fatal(err)
	}
	next := feed.Flashblock{PayloadID: first.PayloadID, Number: 11, Index: 2, Transactions: []feed.FlashTransaction{{Hash: domain.Hash{9}, Successful: false}, {Hash: domain.Hash{10}, Successful: false}}}
	if err := r.acceptFlashblock(t.Context(), next, received); err != nil {
		t.Fatal(err)
	}
	if err := research.Close(); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var prefixes []pipeline.PrefixObservation
	for decoder.More() {
		var row struct {
			Type    string          `json:"type"`
			Details json.RawMessage `json:"details"`
		}
		if err := decoder.Decode(&row); err != nil {
			t.Fatal(err)
		}
		if row.Type != "prefix" {
			continue
		}
		var p pipeline.PrefixObservation
		if err := json.Unmarshal(row.Details, &p); err != nil {
			t.Fatal(err)
		}
		prefixes = append(prefixes, p)
	}
	if len(prefixes) != 3 {
		t.Fatal("missing frame availability", len(prefixes))
	}
	for i, p := range prefixes {
		first, count := uint64(0), uint64(i)
		if i == 2 {
			first = 1
			count = 2
		}
		if p.FrameIndex != uint64(i) || p.FirstTxIndex != first || p.TransactionCount != count || p.ReceivedAtNS != received.UnixNano() || p.AppliedAtNS < p.ReceivedAtNS || p.Version.BlockNumber != 11 {
			t.Fatal("incorrect public frame position", p)
		}
	}
}
