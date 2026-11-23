package backtest

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/state"
)

func captureView(t *testing.T, number uint64) state.View {
	t.Helper()
	store := state.New()
	if err := store.Upsert(testV2Pool(1, 1, 2, 1000, 2000)); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginBlock(domain.BlockRef{Number: number, Hash: domain.Hash{byte(number)}, ParentHash: domain.Hash{byte(number - 1)}}); err != nil {
		t.Fatal(err)
	}
	store.Commit()
	return store.View()
}

func readRecords(t *testing.T, path string) []Record {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var records []Record
	scanner := bufio.NewScanner(file)
	scanner.Buffer(nil, 1<<20)
	for scanner.Scan() {
		var record Record
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// Records queued before a rotation belong to the old file, even when the writer
// has not reached them yet; the new file starts with a replayable snapshot.
func TestCaptureRotationKeepsQueueOrder(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.jsonl"), filepath.Join(dir, "second.jsonl")
	capture, err := NewCapture(first, 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := capture.Snapshot(captureView(t, 5)); err != nil {
		t.Fatal(err)
	}
	for number := uint64(6); number < 26; number++ {
		if err := capture.Block(domain.BlockRef{Number: number}, nil); err != nil {
			t.Fatal(err)
		}
	}
	next, err := os.OpenFile(second, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	retired, err := capture.Rotate(next, captureView(t, 25))
	if err != nil {
		t.Fatal(err)
	}
	for number := uint64(26); number < 36; number++ {
		if err := capture.Block(domain.BlockRef{Number: number}, nil); err != nil {
			t.Fatal(err)
		}
	}
	old := <-retired
	if old.Name() != first {
		t.Fatalf("retired %s, want the first file", old.Name())
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if err := capture.Close(); err != nil {
		t.Fatal(err)
	}
	check := func(path string, snapshot, from, to uint64) {
		t.Helper()
		records := readRecords(t, path)
		if len(records) != int(to-from)+1 || records[0].Type != "snapshot" || records[0].Block.Number != snapshot {
			t.Fatalf("%s: %d records, first %+v", path, len(records), records[0].Block)
		}
		for i, record := range records[1:] {
			if record.Type != "block" || record.Block.Number != from+uint64(i) {
				t.Fatalf("%s record %d: %s %d", path, i+1, record.Type, record.Block.Number)
			}
		}
	}
	check(first, 5, 6, 26)
	check(second, 25, 26, 36)
}
