package remote

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// bidArchive appends, as JSON lines, every bid window as it leaves the
// estimator's retention and every one of our settled outcomes. The estimator
// keeps only two hours of windows, too few for a backtest; the archive keeps
// them for replays of the competition model. A window is written when it
// leaves, so it carries any rewrite by a later outcome. Records are queued
// without blocking and dropped, and counted, when the writer falls behind.
// The writer stops at bidArchiveMaxBytes, or sooner once it would take a
// quarter of the disk space free when it opened: hosts may keep outputs on a
// small root disk, and a full disk stops the instance.
type bidArchive struct {
	records  chan any
	dropped  atomic.Uint64
	maxBytes int64
	done     chan struct{}
}

const (
	bidArchiveQueue    = 4096
	bidArchiveMaxBytes = 4 << 30
)

func bidArchivePath(output string) string { return output + ".bids-archive.jsonl" }

type archivedWindow struct {
	Type    string            `json:"type"`
	StartMS int64             `json:"start_ms"`
	LastMS  int64             `json:"last_ms"`
	Pools   []storedPool      `json:"pools"`
	Worth   uint64            `json:"worth,omitempty"`
	Priced  bool              `json:"priced,omitempty"`
	First   bool              `json:"first,omitempty"`
	FirstBy string            `json:"first_by,omitempty"`
	Contest []archivedContest `json:"contest,omitempty"`
}

type archivedContest struct {
	From string `json:"from"`
	To   string `json:"to,omitempty"`
	Tip  uint64 `json:"tip"`
}

type archivedOutcome struct {
	Type       string       `json:"type"`
	DecisionNS int64        `json:"decision_ns"`
	WindowMS   int64        `json:"window_ms,omitempty"`
	Pools      []storedPool `json:"pools"`
	Worth      uint64       `json:"worth"`
	Tip        string       `json:"tip"`
	ModelTip   string       `json:"model_tip,omitempty"`
	Won        bool         `json:"won"`
	Win        float64      `json:"win"`
	WinRaw     float64      `json:"win_raw"`
	Shift      float64      `json:"shift"`
	AgeNS      int64        `json:"age_ns,omitempty"`
	Lane       string       `json:"lane,omitempty"`
	Block      uint64       `json:"block,omitempty"`
	Frame      uint64       `json:"frame,omitempty"`
}

func openBidArchive(ctx context.Context, path string, maxBytes int64) (*bidArchive, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("bid archive: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, fmt.Errorf("bid archive: %w", err)
	}
	var disk syscall.Statfs_t
	if err := syscall.Statfs(filepath.Dir(path), &disk); err == nil {
		maxBytes = min(maxBytes, info.Size()+int64(disk.Bavail*uint64(disk.Bsize)/4))
	}
	a := &bidArchive{records: make(chan any, bidArchiveQueue), maxBytes: maxBytes, done: make(chan struct{})}
	go a.run(ctx, f, info.Size())
	return a, nil
}

func (a *bidArchive) run(ctx context.Context, f *os.File, size int64) {
	defer close(a.done)
	defer f.Close()
	w := bufio.NewWriter(f)
	full := size >= a.maxBytes
	write := func(record any) {
		if full {
			return
		}
		line, err := json.Marshal(record)
		if err != nil {
			return
		}
		line = append(line, '\n')
		if _, err := w.Write(line); err != nil {
			slog.Warn("bid archive write failed; archiving stops", "error", err)
			full = true
			return
		}
		if size += int64(len(line)); size >= a.maxBytes {
			slog.Warn("bid archive reached its size limit; archiving stops", "bytes", size)
			full = true
		}
	}
	for {
		select {
		case <-ctx.Done():
			for {
				select {
				case record := <-a.records:
					write(record)
				default:
					_ = w.Flush()
					return
				}
			}
		case record := <-a.records:
			write(record)
			if len(a.records) == 0 {
				_ = w.Flush()
			}
		}
	}
}

func (a *bidArchive) offer(record any) {
	select {
	case a.records <- record:
	default:
		a.dropped.Add(1)
	}
}

// window queues a window leaving retention. It runs under the estimator's
// lock, so it copies and never waits.
func (a *bidArchive) window(w bidWindow) {
	record := archivedWindow{Type: "window", StartMS: w.start.UnixMilli(), LastMS: w.last.UnixMilli(), Worth: w.worth, Priced: w.priced, First: w.first}
	if w.first && w.firstBy != (domain.Address{}) {
		record.FirstBy = w.firstBy.Hex()
	}
	for _, key := range w.pools {
		record.Pools = append(record.Pools, encodePool(key))
	}
	for _, c := range w.contest {
		contest := archivedContest{From: c.from.Hex(), Tip: c.tip}
		if c.to != (domain.Address{}) {
			contest.To = c.to.Hex()
		}
		record.Contest = append(record.Contest, contest)
	}
	a.offer(record)
}

// outcome queues one of our settled sends with the decision that priced it.
func (a *bidArchive) outcome(receipt *execution.Settlement) {
	intent := receipt.Intent
	bid := intent.Bid
	if bid == nil {
		return
	}
	record := archivedOutcome{Type: "outcome", DecisionNS: bid.DecisionNS, WindowMS: bid.Window, Worth: bid.Worth, Won: receipt.Success,
		Win: bid.Win, WinRaw: bid.WinRaw, Shift: bid.Shift, AgeNS: bid.AgeNS, Lane: bid.Lane}
	for _, key := range routePools(*intent) {
		record.Pools = append(record.Pools, encodePool(key))
	}
	if intent.Call.TipCap != nil {
		record.Tip = intent.Call.TipCap.String()
	}
	if bid.ModelTip != nil {
		record.ModelTip = bid.ModelTip.String()
	}
	if p := receipt.Position; p != nil {
		record.Block, record.Frame = p.BlockNumber, p.FrameIndex
	}
	a.offer(record)
}
