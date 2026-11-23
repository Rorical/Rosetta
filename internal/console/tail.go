package console

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync/atomic"
	"time"
)

// A single pass reads at most this much, so a large backlog cannot hold one
// transaction open or grow memory with file size.
const tailChunk = 4 << 20

const maxLine = tailChunk

// Tailer imports complete JSONL lines from one instance's run outputs.
type Tailer struct {
	store    *Store
	instance string
	buffer   []byte
	skipped  *atomic.Uint64
	lag      int64
}

// Pass reads every open file once. current is the last run the instance
// reported, empty until one is known. Files of older runs are closed out at
// EOF, because run IDs never repeat and no writer can return to them.
func (t *Tailer) Pass(ctx context.Context, current string) error {
	files, err := t.store.openFiles(ctx, t.instance)
	if err != nil {
		return err
	}
	var failures []error
	t.lag = 0
	for _, file := range files {
		for {
			advanced, err := t.read(ctx, file, current)
			if err != nil {
				failures = append(failures, fmt.Errorf("%s output of run %s: %w", file.Kind, file.RunID, err))
				break
			}
			if advanced == file.Offset {
				break
			}
			file.Offset = advanced
		}
		if info, err := os.Stat(file.Path); err == nil && info.Size() > file.Offset {
			t.lag += info.Size() - file.Offset
		}
	}
	return errors.Join(failures...)
}

func (t *Tailer) read(ctx context.Context, file tailFile, current string) (int64, error) {
	handle, err := os.Open(file.Path)
	if errors.Is(err, os.ErrNotExist) {
		// The bot reports a run's paths before it creates the files.
		if current == "" || file.RunID == current {
			return file.Offset, nil
		}
		slog.Warn("console: output file removed", "kind", file.Kind, "run", file.RunID)
		return file.Offset, t.store.finishFile(ctx, file.Path)
	}
	if err != nil {
		return file.Offset, err
	}
	defer handle.Close()
	// A file shorter than the stored offset was replaced; read the new one whole.
	if info, err := handle.Stat(); err == nil && info.Size() < file.Offset {
		slog.Warn("console: output file shrank; reading from the start", "kind", file.Kind, "run", file.RunID)
		file.Offset = 0
	}
	if t.buffer == nil {
		t.buffer = make([]byte, tailChunk)
	}
	buffer := t.buffer
	count, err := handle.ReadAt(buffer, file.Offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return file.Offset, err
	}
	atEOF := errors.Is(err, io.EOF)
	end := bytes.LastIndexByte(buffer[:count], '\n')
	if end < 0 {
		if count >= maxLine {
			return file.Offset, fmt.Errorf("line at offset %d exceeds %d bytes", file.Offset, maxLine)
		}
		// A retired run may end in a torn line; nothing will ever complete it.
		if atEOF && current != "" && file.RunID != current {
			return file.Offset, t.store.finishFile(ctx, file.Path)
		}
		return file.Offset, nil
	}
	records := decodeLines(file, buffer[:end+1], t.skipped)
	offset := file.Offset + int64(end) + 1
	if err := t.store.commitBatch(ctx, file, offset, records); err != nil {
		return file.Offset, fmt.Errorf("store batch: %w", err)
	}
	return offset, nil
}

// decodeLines skips an undecodable line after logging it. One bad row must not
// stall every later line of an append-only file.
func decodeLines(file tailFile, lines []byte, skipped *atomic.Uint64) *batch {
	records := &batch{}
	parse := parseReactive
	switch file.Kind {
	case kindCompetition:
		parse = parseCompetitor
	case kindCandidate:
		parse = parseCandidate
	}
	for line := range bytes.SplitSeq(lines, []byte{'\n'}) {
		if len(line) == 0 {
			continue
		}
		if err := parse(line, records); err != nil {
			slog.Warn("console: skip output line", "kind", file.Kind, "run", file.RunID, "error", err)
			if skipped != nil {
				skipped.Add(1)
			}
		}
	}
	return records
}

func tailBackoff(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
