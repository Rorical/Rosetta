package remote

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// preflightStoreFormat versions the saved passes. A file in another format,
// or one that does not parse, is ignored: losing passes costs one preflight
// per route, never an unsimulated send.
const preflightStoreFormat = 1

type preflightFile struct {
	Format int             `json:"format"`
	Passes []preflightPass `json:"passes"`
	// Refuted and RefutedLegs keep reverts fail-closed across restarts; a file
	// written before they existed restores none.
	Refuted     []preflightPass `json:"refuted,omitempty"`
	RefutedLegs []preflightPass `json:"refuted_legs,omitempty"`
}

type preflightPass struct {
	Route string `json:"route"`
	AtMS  int64  `json:"at_ms"`
}

func preflightStorePath(output string) string { return output + ".preflight.json" }

func (p *reactivePreflight) load(now time.Time) {
	if p.store == "" {
		return
	}
	raw, err := os.ReadFile(p.store)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	var file preflightFile
	if err == nil {
		err = json.Unmarshal(raw, &file)
	}
	if err == nil && file.Format != preflightStoreFormat {
		err = fmt.Errorf("format %d", file.Format)
	}
	if err != nil {
		slog.Warn("preflight passes not restored", "path", p.store, "error", err)
		return
	}
	for _, pass := range file.Passes {
		p.pass(p.rekey(pass.Route), time.UnixMilli(pass.AtMS))
	}
	p.mu.Lock()
	// After the passes, whose pass() clears refutations: these were current.
	if p.refuted == nil {
		p.refuted = make(map[string]time.Time)
	}
	if p.refutedLegs == nil {
		p.refutedLegs = make(map[string]time.Time)
	}
	for _, r := range file.Refuted {
		insertBounded(p.refuted, p.rekey(r.Route), time.UnixMilli(r.AtMS), preflightPassEntries)
	}
	for _, r := range file.RefutedLegs {
		insertBounded(p.refutedLegs, p.rekey(r.Route), time.UnixMilli(r.AtMS), preflightPassEntries)
	}
	p.dirty = false
	p.mu.Unlock()
	slog.Info("preflight passes restored", "path", p.store, "routes", len(file.Passes))
}

// save writes the passes when they changed. It runs on the diagnosis
// goroutine, off the send path.
func (p *reactivePreflight) save() {
	if p.store == "" {
		return
	}
	p.mu.Lock()
	if !p.dirty {
		p.mu.Unlock()
		return
	}
	file := preflightFile{Format: preflightStoreFormat, Passes: make([]preflightPass, 0, len(p.passed))}
	for route, at := range p.passed {
		file.Passes = append(file.Passes, preflightPass{Route: route, AtMS: at.UnixMilli()})
	}
	for route, at := range p.refuted {
		file.Refuted = append(file.Refuted, preflightPass{Route: route, AtMS: at.UnixMilli()})
	}
	for leg, at := range p.refutedLegs {
		file.RefutedLegs = append(file.RefutedLegs, preflightPass{Route: leg, AtMS: at.UnixMilli()})
	}
	p.dirty = false
	p.mu.Unlock()
	raw, err := json.Marshal(file)
	if err == nil {
		err = writeFileAtomic(p.store, raw)
	}
	if err != nil {
		slog.Warn("preflight passes not saved", "path", p.store, "error", err)
		p.mu.Lock()
		p.dirty = true
		p.mu.Unlock()
	}
}
