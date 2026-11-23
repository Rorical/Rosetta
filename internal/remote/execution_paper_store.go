package remote

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"

	"github.com/Rorical/Rosetta/internal/domain"
)

// A locked append-only journal survives run rotation. Only a torn final line
// is removed after a crash; completed samples and their provenance stay intact.
func openPaperJournal(path string, chain domain.ChainIdentity, artifact domain.Hash) (*os.File, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0600)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*os.File, error) { f.Close(); return nil, err }
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fail(fmt.Errorf("execution paper journal locked: %w", err))
	}
	info, err := f.Stat()
	if err != nil {
		return fail(err)
	}
	if info.Size() > 0 {
		var header paperExecutionResult
		line, err := bufio.NewReader(io.LimitReader(f, 1<<20)).ReadBytes('\n')
		if err != nil {
			return fail(err)
		}
		if err := json.Unmarshal(line, &header); err != nil {
			return fail(err)
		}
		if header.Type != "session" || header.Schema != 1 || header.Chain != chain || header.Artifact != artifact || header.Basis != "executor_simulation" {
			return fail(fmt.Errorf("execution paper journal identity mismatch; use a new output for changed chain/artifact"))
		}
		tail := make([]byte, min(info.Size(), 1<<20))
		if _, err := f.ReadAt(tail, info.Size()-int64(len(tail))); err != nil {
			return fail(err)
		}
		last := -1
		for i := len(tail) - 1; i >= 0; i-- {
			if tail[i] == '\n' {
				last = i
				break
			}
		}
		if last < 0 {
			return fail(fmt.Errorf("execution paper journal has no complete record"))
		}
		if err := f.Truncate(info.Size() - int64(len(tail)) + int64(last) + 1); err != nil {
			return fail(err)
		}

	} else {
		if err := json.NewEncoder(f).Encode(paperExecutionResult{Type: "session", Schema: 1, Basis: "executor_simulation", Chain: chain, Artifact: artifact}); err != nil {
			return fail(err)
		}
	}
	if err := f.Sync(); err != nil {
		return fail(err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fail(err)
	}
	err = directory.Sync()
	directory.Close()
	if err != nil {
		return fail(err)
	}
	return f, nil
}
