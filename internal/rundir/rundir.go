// Package rundir names and creates the per-run output directories of a
// long-running instance. The layout matches what the paper maintenance script
// reads: ROOT/runs/<UTC stamp>-<pid>/ and a ROOT/current-run marker.
package rundir

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

const (
	runsDir    = "runs"
	markerFile = "current-run"
	stampTime  = "20060102T150405"
)

// Run is one output directory. Its files are created by their writers.
type Run struct{ Dir string }

func (r Run) Candidates() string { return filepath.Join(r.Dir, "candidates.jsonl") }
func (r Run) Research() string   { return filepath.Join(r.Dir, "observations.jsonl") }
func (r Run) Replay() string     { return filepath.Join(r.Dir, "replay.jsonl") }
func (r Run) Pools() string      { return filepath.Join(r.Dir, "final-pools.json") }
func (r Run) Metrics() string    { return filepath.Join(r.Dir, "final-metrics.json") }

// Copy is a file placed in every new run, such as the configuration the
// process loaded, so a run directory records what produced it.
type Copy struct {
	Name string
	Data []byte
}

// Root creates runs under one state directory.
type Root struct {
	dir    string
	copies []Copy
}

func Open(dir string, copies []Copy) (*Root, error) {
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return nil, fmt.Errorf("resolve run root: %w", err)
	}
	if err := os.MkdirAll(filepath.Join(absolute, runsDir), 0o700); err != nil {
		return nil, fmt.Errorf("create runs directory: %w", err)
	}
	return &Root{dir: absolute, copies: copies}, nil
}

// Create makes a new run directory named <UTC stamp>-<pid>, as the start
// script did. A second run within the same second of one process gets a
// counter suffix; names never repeat.
func (r *Root) Create(now time.Time) (Run, error) {
	base := filepath.Join(r.dir, runsDir, now.UTC().Format(stampTime)+"-"+strconv.Itoa(os.Getpid()))
	run := Run{Dir: base}
	err := os.Mkdir(run.Dir, 0o700)
	for sequence := 2; errors.Is(err, fs.ErrExist) && sequence < 100; sequence++ {
		run.Dir = base + "-" + strconv.Itoa(sequence)
		err = os.Mkdir(run.Dir, 0o700)
	}
	if err != nil {
		return Run{}, fmt.Errorf("create run directory: %w", err)
	}
	for _, saved := range r.copies {
		if err := os.WriteFile(filepath.Join(run.Dir, saved.Name), saved.Data, 0o600); err != nil {
			return Run{}, errors.Join(fmt.Errorf("write run %s: %w", saved.Name, err), r.Remove(run))
		}
	}
	return run, nil
}

// Remove deletes a run that never received output.
func (r *Root) Remove(run Run) error {
	return os.RemoveAll(run.Dir)
}

// Mark names run in ROOT/current-run. The replacement is atomic, so a reader
// sees the previous run or this one, never a partial path.
func (r *Root) Mark(run Run) error {
	marker := filepath.Join(r.dir, markerFile)
	temporary := marker + ".next"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write current-run marker: %w", err)
	}
	_, err = file.WriteString(run.Dir + "\n")
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return fmt.Errorf("write current-run marker: %w", err)
	}
	if err := os.Rename(temporary, marker); err != nil {
		return fmt.Errorf("replace current-run marker: %w", err)
	}
	return nil
}
