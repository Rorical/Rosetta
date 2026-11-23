package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

func readArchive(t *testing.T, path string) []map[string]any {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var records []map[string]any
	scan := bufio.NewScanner(f)
	for scan.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scan.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		records = append(records, record)
	}
	return records
}

// A window reaches the archive when it leaves retention, with the contract
// each rival called; a settled send follows with the decision that priced it.
func TestBidArchiveKeepsWindowsPastRetentionAndOutcomes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.bids-archive.jsonl")
	ctx, cancel := context.WithCancel(t.Context())
	a, err := openBidArchive(ctx, path, bidArchiveMaxBytes)
	if err != nil {
		t.Fatal(err)
	}
	e := newBidEstimator(250*time.Millisecond, 550*time.Millisecond)
	e.archive = a.window
	start := time.Unix(1_800_000_000, 0)
	bot, sender := domain.Address{0x65, 0x61}, domain.Address{0x01}
	old := bidWindow{pools: route, start: start, last: start, worth: 1_000_000, priced: true}
	old.contend(sender, bot, 1_000_008)
	e.addWindow(old)
	later := bidWindow{pools: route, start: start.Add(bidRetention + time.Minute), last: start.Add(bidRetention + time.Minute)}
	e.addWindow(later)
	intent := &execution.ReactiveIntent{Bid: &execution.BidDecision{DecisionNS: 7, Worth: 900_000, Win: 0.4, WinRaw: 0.45, Lane: "fast", ModelTip: big.NewInt(50_000)}}
	intent.Call.TipCap = big.NewInt(50_000)
	intent.Route.Steps = []execution.ExecutorStep{{Pool: route[0]}, {Pool: route[1]}}
	a.outcome(&execution.Settlement{Intent: intent, Success: false, Position: &execution.ExecutionPosition{BlockNumber: 9, FrameIndex: 3}})
	cancel()
	<-a.done
	records := readArchive(t, path)
	if len(records) != 2 {
		t.Fatalf("%d records, want the retired window and the outcome: %v", len(records), records)
	}
	window, outcome := records[0], records[1]
	contest := window["contest"].([]any)[0].(map[string]any)
	if window["type"] != "window" || window["start_ms"].(float64) != float64(start.UnixMilli()) || contest["to"] != bot.Hex() || contest["tip"].(float64) != 1_000_008 {
		t.Fatalf("window record %v", window)
	}
	if outcome["type"] != "outcome" || outcome["tip"] != "50000" || outcome["won"] != false || outcome["lane"] != "fast" || len(outcome["pools"].([]any)) != 2 || outcome["block"].(float64) != 9 {
		t.Fatalf("outcome record %v", outcome)
	}
}

// The writer stops at its size limit rather than fill the disk.
func TestBidArchiveStopsAtItsSizeLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out.bids-archive.jsonl")
	ctx, cancel := context.WithCancel(t.Context())
	a, err := openBidArchive(ctx, path, 300)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 20 {
		a.window(bidWindow{pools: route, start: time.Unix(int64(i), 0), last: time.Unix(int64(i), 0)})
	}
	cancel()
	<-a.done
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(readArchive(t, path)); n == 0 || n >= 20 || info.Size() > 600 {
		t.Fatalf("%d records, %d bytes past a 300-byte limit", n, info.Size())
	}
}

// The called contract survives a restart through the bid store.
func TestBidStoreKeepsTheCalledContract(t *testing.T) {
	e := newBidEstimator(250*time.Millisecond, 550*time.Millisecond)
	now := time.Now()
	w := bidWindow{pools: route, start: now.Add(-time.Minute), last: now.Add(-time.Minute), worth: 5}
	w.contend(domain.Address{1}, domain.Address{2}, 9)
	e.addWindow(w)
	raw, err := e.encode(now)
	if err != nil {
		t.Fatal(err)
	}
	restored := newBidEstimator(250*time.Millisecond, 550*time.Millisecond)
	if err := restored.decode(raw, now); err != nil {
		t.Fatal(err)
	}
	if len(restored.windows) != 1 || restored.windows[0].contest[0].to != (domain.Address{2}) {
		t.Fatalf("contract lost: %+v", restored.windows)
	}
}
