package remote

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestFeedArrivalLogWritesEachMessage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "arrivals.jsonl")
	l, err := openFeedArrivalLog(path)
	if err != nil {
		t.Fatal(err)
	}
	l.record(7, domain.Hash{1}, time.Unix(0, 42))
	l.record(8, domain.Hash{2}, time.Unix(0, 43))
	l.close()
	var nilLog *feedArrivalLog
	nilLog.record(9, domain.Hash{}, time.Now()) // disabled log
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []feedArrivalRow
	for dec := json.NewDecoder(bytes.NewReader(raw)); dec.More(); {
		var row feedArrivalRow
		if err := dec.Decode(&row); err != nil {
			t.Fatal(err)
		}
		rows = append(rows, row)
	}
	if len(rows) != 2 || rows[0].Sequence != 7 || rows[1].AtNS != 43 || rows[0].BlockHash != (domain.Hash{1}).Hex() {
		t.Fatal("rows", rows)
	}
}
