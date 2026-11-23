package pipeline

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Observations keep flowing from the research worker while the file under it
// changes. Every record lands exactly once, in order, and each file opens with
// its own session header.
func TestResearchSwapWhileWriting(t *testing.T) {
	dir := t.TempDir()
	paths := []string{filepath.Join(dir, "0.jsonl")}
	r, err := NewResearch(t.Context(), "", paths[0], domain.ChainIdentity{ChainID: 10})
	if err != nil {
		t.Fatal(err)
	}
	const total = 3000
	var sent atomic.Int64
	produced := make(chan struct{})
	go func() {
		defer close(produced)
		for i := range total {
			sent.Store(int64(i))
			for len(r.confirmations) >= cap(r.confirmations)-1 {
				time.Sleep(10 * time.Microsecond)
			}
			r.Confirmation(ConfirmationEvent{Err: errors.New("sample-" + strconv.Itoa(i))})
		}
	}()
	var retired []*os.File
	for rotation := 1; rotation <= 4; rotation++ {
		for sent.Load() < int64(rotation*total/5) {
			time.Sleep(10 * time.Microsecond)
		}
		path := filepath.Join(dir, fmt.Sprintf("%d.jsonl", rotation))
		next, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.StartFile(next, time.Now()); err != nil {
			t.Fatal(err)
		}
		retired = append(retired, r.Swap(next))
		paths = append(paths, path)
	}
	<-produced
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range retired {
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if skipped := r.skipped.Load(); skipped != 0 {
		t.Fatalf("producer outran the worker: %d skipped", skipped)
	}
	next, filled := 0, 0
	for _, path := range paths {
		before := next
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(file)
		for line := 0; decoder.More(); line++ {
			var event ResearchEvent
			if err := decoder.Decode(&event); err != nil {
				t.Fatalf("%s line %d: %v", path, line, err)
			}
			if (line == 0) != (event.Type == "session") || event.ChainID != 10 {
				t.Fatalf("%s line %d: %s chain %d", path, line, event.Type, event.ChainID)
			}
			if event.Type != "confirmation" {
				continue
			}
			details, _ := event.Details.(map[string]any)
			sample, _ := details["error"].(string)
			if sample != "sample-"+strconv.Itoa(next) {
				t.Fatalf("%s line %d: %q, want sample-%d", path, line, sample, next)
			}
			next++
		}
		file.Close()
		if next > before {
			filled++
		}
	}
	if filled < 2 {
		t.Fatal("every sample landed in one file; the swap never raced the writer")
	}
	if next != total {
		t.Fatalf("%d of %d samples written", next, total)
	}
	if !strings.HasSuffix(r.output.current().Name(), "4.jsonl") {
		t.Fatal("close did not reach the newest file")
	}
}
