package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Rorical/Rosetta/internal/config"
	"github.com/Rorical/Rosetta/internal/control"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/rundir"
	poolstate "github.com/Rorical/Rosetta/internal/state"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// runRotation owns the run directories of an instance started with -run-root.
// Candidates, observations and replay start fresh in every run; the reactive
// output, competition file and paper journal are appended across runs.
type runRotation struct {
	root    *rundir.Root
	run     rundir.Run
	closing sync.WaitGroup
	busy    atomic.Bool // a closed run's files are still being finished
}

// openRunRoot creates the first run, points cfg's per-run outputs into it and
// publishes it. It runs after the control socket's lock is held, so a second
// instance cannot move the current-run marker.
func openRunRoot(ctx context.Context, cfg *config.Config) (*runRotation, error) {
	var copies []rundir.Copy
	for _, source := range []struct{ name, path string }{{"search.json", cfg.SearchConfig}, {"research.json", cfg.ResearchConfig}} {
		if source.path == "" {
			continue
		}
		data, err := os.ReadFile(source.path)
		if err != nil {
			return nil, fmt.Errorf("copy run configuration: %w", err)
		}
		copies = append(copies, rundir.Copy{Name: source.name, Data: data})
	}
	root, err := rundir.Open(cfg.RunRoot, copies)
	if err != nil {
		return nil, err
	}
	run, err := root.Create(time.Now())
	if err != nil {
		return nil, err
	}
	if err := root.Mark(run); err != nil {
		return nil, err
	}
	if _, err := control.StartRun(ctx, run.Candidates()); err != nil {
		return nil, err
	}
	runs := &runRotation{root: root, run: run}
	runs.apply(cfg)
	return runs, nil
}

func (rr *runRotation) apply(cfg *config.Config) {
	cfg.CandidateOut, cfg.ResearchOut, cfg.ReplayOut, cfg.PoolSnapshotOut = rr.run.Candidates(), rr.run.Research(), rr.run.Replay(), rr.run.Pools()
}

// close runs after every worker has joined, like the -metrics-out sink: it
// waits for closing runs and writes the last run's final metrics.
func (rr *runRotation) close(registry *telemetry.Registry) error {
	rr.closing.Wait()
	return registry.WriteFile(rr.run.Metrics())
}

type runFiles struct{ candidates, research, replay *os.File }

func (f runFiles) close() {
	for _, file := range []*os.File{f.candidates, f.research, f.replay} {
		if file != nil {
			file.Close()
		}
	}
}

// prepare creates the next run with its outputs open and the observation
// header written, so the switch itself cannot fail halfway.
func (rr *runRotation) prepare(research *pipeline.Research, now time.Time) (rundir.Run, runFiles, error) {
	run, err := rr.root.Create(now)
	if err != nil {
		return rundir.Run{}, runFiles{}, err
	}
	var files runFiles
	for _, output := range []struct {
		file **os.File
		path string
	}{{&files.candidates, run.Candidates()}, {&files.research, run.Research()}, {&files.replay, run.Replay()}} {
		if *output.file, err = os.OpenFile(output.path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
			break
		}
	}
	if err == nil {
		err = research.StartFile(files.research, now)
	}
	if err != nil {
		files.close()
		return rundir.Run{}, runFiles{}, errors.Join(err, rr.root.Remove(run))
	}
	return run, files, nil
}

// rotate runs on the main loop between two events, so the closed run ends at
// one pool state: its replay and final snapshot stop at the same block, and
// no search, feed or state work is interrupted. Only the switch happens here.
// Syncing the closed files and encoding the snapshot run in the background,
// and the requester hears back once the closed run is complete on disk.
func (r *runtime) rotate(ctx context.Context, request control.Rotation) {
	rr := r.runs
	if !rr.busy.CompareAndSwap(false, true) {
		request.Finish(control.RotationResult{}, errors.New("the previous rotation is still closing its run"))
		return
	}
	fail := func(err error) {
		rr.busy.Store(false)
		slog.Error("run rotation refused; outputs unchanged", "run", rr.run.Dir, "error", err)
		request.Finish(control.RotationResult{}, err)
	}
	next, files, err := rr.prepare(r.research, time.Now())
	if err != nil {
		fail(err)
		return
	}
	// The capture queue is the only step that can refuse; everything after it
	// commits the rotation.
	retiredReplay, err := r.capture.Rotate(files.replay, r.store.View())
	if err != nil {
		files.close()
		fail(errors.Join(err, rr.root.Remove(next)))
		return
	}
	closed := rr.run
	retiredCandidates := r.candidateFile
	r.candidateFile, r.candidates = files.candidates, json.NewEncoder(files.candidates)
	retiredResearch := r.research.Swap(files.research)
	checkpoint := r.store.Checkpoint()
	rr.run = next
	rr.apply(&r.cfg)
	r.snapshotWritten = false
	runID, published := control.StartRun(ctx, next.Candidates())
	registry := r.metrics
	rr.closing.Go(func() {
		defer rr.busy.Store(false)
		// The marker moves first so a shutdown now finishes the new run. The
		// snapshot follows the data files: its presence means they are closed.
		err := errors.Join(published, rr.root.Mark(next),
			syncClose(retiredCandidates), syncClose(retiredResearch), syncClose(<-retiredReplay),
			writeCheckpoint(closed.Pools(), checkpoint), registry.WriteFile(closed.Metrics()))
		if err != nil {
			slog.Error("run rotated with errors", "closed", closed.Dir, "run", next.Dir, "error", err)
		} else {
			slog.Info("run rotated", "closed", closed.Dir, "run", next.Dir, "run_id", runID)
		}
		request.Finish(control.RotationResult{Closed: closed.Dir, Run: next.Dir, RunID: runID}, err)
	})
}

func syncClose(file *os.File) error {
	return errors.Join(file.Sync(), file.Close())
}

// writeCheckpoint publishes the closed run's snapshot by rename. Maintenance
// treats its presence as "the run is complete", so it never appears partial.
func writeCheckpoint(path string, checkpoint poolstate.Snapshot) error {
	data, err := checkpoint.Encode()
	if err != nil {
		return err
	}
	temporary := path + ".next"
	file, err := os.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = file.Write(data)
	if err = errors.Join(err, file.Sync(), file.Close()); err != nil {
		return err
	}
	return os.Rename(temporary, path)
}
