package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

const (
	// bidStoreEvery bounds what a crash loses; shutdown saves as well.
	bidStoreEvery = 30 * time.Second
	// bidStoreMaxAge drops history that no longer describes the rivals.
	bidStoreMaxAge = 6 * time.Hour
	// Format 5 adds separate revert costs and decision age/lane calibration.
	// Format 4 keeps successful-execution gas only. Older gas samples mixed
	// early reverts with successes and cannot be classified after restart.
	// Format 1 kept each window's single best bid and per-sender bid ratios;
	// format 2 keeps every contest of a window and our calibration outcomes;
	// format 3 adds each outcome's worth. A format 1 file is migrated: each
	// sample becomes a window with at most one contest, and the ratios, which
	// no longer inform a bid, are dropped. Format 2 outcomes load without a
	// worth, so they fit the calibration's intercept and not its slope.
	bidStoreFormat       = 5
	bidStoreFormat4      = 4
	bidStoreFormat3      = 3
	bidStoreFormat2      = 2
	bidStoreLegacyFormat = 1
)

// bidFile is the estimator's state across restarts: without it every restart
// empties the estimator and sends are bid by the fallback share.
// Open windows and pending outcomes are dropped. Pools are stored once and
// windows refer to them by index.
type bidFile struct {
	Format      int                `json:"format"`
	SavedMS     int64              `json:"saved_ms"`
	Pools       []storedPool       `json:"pools"`
	Windows     []storedWindow     `json:"windows,omitempty"`
	Samples     []storedSample     `json:"samples,omitempty"` // format 1
	RevertGas   []float64          `json:"revert_gas,omitempty"`
	Gas         []float64          `json:"gas,omitempty"`
	Calibration []storedCalibrated `json:"calibration,omitempty"`
	// Sent are our own sends and the tip that would have won each (bid_sent.go).
	Sent []storedSent `json:"sent,omitempty"`

	// RevertGasConditional holds conditional losses; zero is a dropped send.
	RevertGasConditional []float64 `json:"revert_gas_conditional,omitempty"`
	// RouteGas is the gas routes used, by route and by pool kinds (bid_route_gas.go).
	RouteGas *storedRouteGas `json:"route_gas,omitempty"`
}

type storedPool struct {
	Address string          `json:"a"`
	Kind    domain.PoolKind `json:"k"`
	ID      string          `json:"id,omitempty"`
	Hooks   string          `json:"h,omitempty"`
}

type storedWindow struct {
	AtMS    int64           `json:"t"`
	Pools   []int           `json:"p"`
	Worth   uint64          `json:"w,omitempty"`
	First   bool            `json:"f,omitempty"`
	FirstBy string          `json:"fb,omitempty"`
	Contest []storedContest `json:"c,omitempty"`
}

type storedContest struct {
	From string `json:"f"`
	To   string `json:"to,omitempty"`
	Tip  uint64 `json:"b"`
}

// storedSample is a format 1 window: Bid is the best rival bid, 0 when
// quiet, MaxUint64 when a rival landed first.
type storedSample struct {
	AtMS  int64  `json:"t"`
	Pools []int  `json:"p"`
	Bid   uint64 `json:"b"`
	From  string `json:"f,omitempty"`
	Worth uint64 `json:"w,omitempty"`
}

// storedSent is a sentOutcome; Beat is absent for maxBid.
type storedSent struct {
	AtMS  int64   `json:"t"`
	Worth uint64  `json:"w"`
	Beat  *uint64 `json:"b,omitempty"`
}

type storedCalibrated struct {
	AtMS  int64   `json:"t"`
	Raw   float64 `json:"p"`
	AgeNS int64   `json:"age_ns,omitempty"`
	Lane  string  `json:"lane,omitempty"`
	Worth uint64  `json:"w,omitempty"` // format 3
	Won   bool    `json:"y,omitempty"`
}

// bidStorePath keeps the history next to the reactive output it belongs to.
func bidStorePath(output string) string { return output + ".bids.json" }

// encode writes the windows, gas ratios and outcomes oldest first. It copies
// them under mu and encodes outside it: two hours of windows take tens of
// milliseconds to marshal, and the feed loop waits on mu. The copy shares
// each window's contests, which resolve replaces rather than edits.
func (e *bidEstimator) encode(now time.Time) ([]byte, error) {
	e.mu.Lock()
	windows := slices.Clone(e.windows)
	gas, revertGas := ringOrder(e.gasRatios, e.gasNext), ringOrder(e.revertRatios, e.revertNext)
	conditional := ringOrder(e.conditionalRatios, e.conditionalNext)
	calibration := e.calibration.ordered()
	sent := slices.Clone(e.sent)
	routeGas := e.routeGas.stored()
	e.mu.Unlock()
	file := bidFile{Format: bidStoreFormat, SavedMS: now.UnixMilli(), Gas: gas, RevertGas: revertGas, RevertGasConditional: conditional, RouteGas: routeGas}
	index := map[domain.PoolKey]int{}
	for _, w := range windows {
		stored := storedWindow{AtMS: w.start.UnixMilli(), Worth: w.worth, First: w.first}
		if w.first && w.firstBy != (domain.Address{}) {
			stored.FirstBy = w.firstBy.Hex()
		}
		for _, c := range w.contest {
			contest := storedContest{From: c.from.Hex(), Tip: c.tip}
			if c.to != (domain.Address{}) {
				contest.To = c.to.Hex()
			}
			stored.Contest = append(stored.Contest, contest)
		}
		for _, key := range w.pools {
			i, ok := index[key]
			if !ok {
				i = len(file.Pools)
				index[key] = i
				file.Pools = append(file.Pools, encodePool(key))
			}
			stored.Pools = append(stored.Pools, i)
		}
		file.Windows = append(file.Windows, stored)
	}
	for _, p := range calibration {
		file.Calibration = append(file.Calibration, storedCalibrated{AtMS: p.at.UnixMilli(), Raw: p.raw, Worth: p.worth, Won: p.won, AgeNS: p.ageNS, Lane: p.lane})
	}
	for _, o := range sent {
		stored := storedSent{AtMS: o.at.UnixMilli(), Worth: o.worth}
		if o.beat < maxBid {
			stored.Beat = &o.beat
		}
		file.Sent = append(file.Sent, stored)
	}
	return json.Marshal(file)
}

// decode replaces the estimator's history with a saved one, keeping what is
// younger than bidStoreMaxAge. Any malformed entry rejects the whole file.
func (e *bidEstimator) decode(raw []byte, now time.Time) error {
	var file bidFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return err
	}
	if file.Format != bidStoreFormat && file.Format != bidStoreFormat4 && file.Format != bidStoreFormat3 && file.Format != bidStoreFormat2 && file.Format != bidStoreLegacyFormat {
		return fmt.Errorf("bid store format %d", file.Format)
	}
	cutoff := now.Add(-bidStoreMaxAge)
	pools := make([]domain.PoolKey, len(file.Pools))
	for i, stored := range file.Pools {
		key, err := decodePool(stored)
		if err != nil {
			return err
		}
		pools[i] = key
	}
	stored := file.Windows
	if file.Format == bidStoreLegacyFormat {
		stored = migrateSamples(file.Samples)
	}
	var windows []bidWindow
	for _, s := range stored {
		w := bidWindow{start: time.UnixMilli(s.AtMS), worth: s.Worth, first: s.First}
		w.last = w.start
		var err error
		if s.FirstBy != "" {
			if w.firstBy, err = domain.ParseAddress(s.FirstBy); err != nil {
				return err
			}
		}
		for _, c := range s.Contest {
			from, err := domain.ParseAddress(c.From)
			if err != nil {
				return err
			}
			var to domain.Address
			if c.To != "" {
				if to, err = domain.ParseAddress(c.To); err != nil {
					return err
				}
			}
			w.contend(from, to, c.Tip)
		}
		for _, i := range s.Pools {
			if i < 0 || i >= len(pools) {
				return fmt.Errorf("bid window pool %d of %d", i, len(pools))
			}
			w.pools = append(w.pools, pools[i])
		}
		if w.start.After(cutoff) {
			windows = append(windows, w)
		}
	}
	for _, ratio := range append(slices.Clone(file.Gas), file.RevertGas...) {
		if !validRatio(ratio) || ratio > 1 {
			return fmt.Errorf("gas ratio %v", ratio)
		}
	}
	for _, ratio := range file.RevertGasConditional {
		if math.IsNaN(ratio) || ratio < 0 || ratio > 1 {
			return fmt.Errorf("conditional gas ratio %v", ratio)
		}
	}
	var points []calibrationPoint
	for _, p := range file.Calibration {
		if p.AgeNS < 0 || p.AgeNS > int64(2*time.Second) {
			return fmt.Errorf("invalid calibration age")
		}
		if math.IsNaN(p.Raw) || p.Raw < 0 || p.Raw > 1 {
			return fmt.Errorf("calibration probability %v", p.Raw)
		}
		if at := time.UnixMilli(p.AtMS); at.After(cutoff) {
			points = append(points, calibrationPoint{raw: p.Raw, worth: p.Worth, won: p.Won, at: at, ageNS: p.AgeNS, lane: p.Lane})
		}
	}
	var sent []sentOutcome
	for _, s := range file.Sent[max(0, len(file.Sent)-sentOutcomes):] {
		o := sentOutcome{at: time.UnixMilli(s.AtMS), worth: s.Worth, beat: maxBid}
		if s.Beat != nil {
			o.beat = min(*s.Beat, maxBid)
		}
		if s.Worth != 0 && o.at.After(cutoff) {
			sent = append(sent, o)
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.open, e.pending, e.windows, e.stats = nil, nil, nil, nil
	e.sent = sent
	for _, w := range windows[max(0, len(windows)-bidWindowLimit):] {
		e.addWindow(w)
	}
	e.gasRatios, e.gasNext = nil, 0
	e.gasRatio.Store(0)
	if file.Format >= bidStoreFormat4 {
		for _, ratio := range file.Gas[max(0, len(file.Gas)-gasRatios):] {
			e.addGas(ratio)
		}
	}
	e.revertRatios = slices.Clone(file.RevertGas[max(0, len(file.RevertGas)-gasRatios):])
	e.revertNext = 0
	e.conditionalRatios = slices.Clone(file.RevertGasConditional[max(0, len(file.RevertGasConditional)-gasRatios):])
	e.conditionalNext = 0
	e.routeGas.restore(file.RouteGas)
	e.calibration = calibration{}
	for _, p := range points[max(0, len(points)-calibrationPoints):] {
		e.calibration.points = append(e.calibration.points, p)
	}
	e.calibration.fit()
	return nil
}

// migrateSamples turns format 1 samples into windows. A sample's best bid
// becomes its only contest; a first lander's sender was not kept.
func migrateSamples(samples []storedSample) []storedWindow {
	windows := make([]storedWindow, len(samples))
	for i, s := range samples {
		w := storedWindow{AtMS: s.AtMS, Pools: s.Pools, Worth: s.Worth}
		switch {
		case s.Bid == math.MaxUint64:
			w.First = true
		case s.Bid > 0:
			from := s.From
			if from == "" {
				from = domain.Address{}.Hex()
			}
			w.Contest = []storedContest{{From: from, Tip: s.Bid}}
		}
		windows[i] = w
	}
	return windows
}

// save writes the history atomically: a crash leaves the old file or the new.
func (e *bidEstimator) save(path string, now time.Time) error {
	raw, err := e.encode(now)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, raw)
}

// writeFileAtomic replaces path with raw so a crash leaves the old or the new
// file, never a torn one.
func writeFileAtomic(path string, raw []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if _, err = temp.Write(raw); err == nil {
		err = temp.Sync()
	}
	if closeErr := temp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}

// load restores a saved history. A missing file is a first start; an
// unreadable or corrupt one is logged and the estimator starts empty, since
// the bid falls back to the share of surplus until windows accumulate.
func (e *bidEstimator) load(path string, now time.Time) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return
	}
	if err == nil {
		err = e.decode(raw, now)
	}
	if err != nil {
		slog.Warn("bid history not restored", "path", path, "error", err)
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	slog.Info("bid history restored", "path", path, "windows", len(e.windows), "outcomes", len(e.calibration.points), "gas_ratios", len(e.gasRatios))
}

// persist saves every bidStoreEvery and once more when ctx ends. It runs on
// its own goroutine so the fsync never delays a send.
func (e *bidEstimator) persist(ctx context.Context, path string, done chan<- struct{}) {
	defer close(done)
	ticker := time.NewTicker(bidStoreEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			if err := e.save(path, time.Now()); err != nil {
				slog.Warn("bid history not saved at shutdown", "path", path, "error", err)
			}
			return
		case now := <-ticker.C:
			if err := e.save(path, now); err != nil {
				slog.Warn("bid history not saved", "path", path, "error", err)
			}
		}
	}
}

func encodePool(key domain.PoolKey) storedPool {
	stored := storedPool{Address: key.Address.Hex(), Kind: key.Kind}
	if key.PoolID != (domain.Hash{}) {
		stored.ID = key.PoolID.Hex()
	}
	if key.Hooks != (domain.Address{}) {
		stored.Hooks = key.Hooks.Hex()
	}
	return stored
}

func decodePool(stored storedPool) (domain.PoolKey, error) {
	address, err := domain.ParseAddress(stored.Address)
	if err != nil {
		return domain.PoolKey{}, err
	}
	key := domain.PoolKey{Address: address, Kind: stored.Kind}
	if stored.ID != "" {
		if key.PoolID, err = domain.ParseHash(stored.ID); err != nil {
			return domain.PoolKey{}, err
		}
	}
	if stored.Hooks != "" {
		if key.Hooks, err = domain.ParseAddress(stored.Hooks); err != nil {
			return domain.PoolKey{}, err
		}
	}
	return key, nil
}

func validRatio(ratio float64) bool {
	return ratio > 0 && !math.IsInf(ratio, 0) && !math.IsNaN(ratio)
}

// ringOrder returns a ring buffer's entries oldest first.
func ringOrder[T any](ring []T, next int) []T {
	return append(slices.Clone(ring[next:]), ring[:next]...)
}
