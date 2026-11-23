package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

// CreationIndex retains complete creation-log ranges across scans and restarts.
// The owner verifies Anchor against its canonical source before using a loaded
// index, and resets it on reorg. It is never a cache for mutable pool state.
type CreationIndex struct {
	Source   LogSource
	Path     string
	ChainID  uint64
	Identity domain.Hash
	mu       sync.Mutex
	epoch    uint64
	anchor   domain.BlockRef
	ranges   map[string]logWindow
	records  []Creation
	activity []ActivityRecord
	report   []Assessment
	verdicts []Verdict
	probes   []Cooldown
	v4scans  []V4ScanState
	judged   domain.Hash // VerdictIdentity the verdicts were made under
}

// Verdict is a pool's verification outcome, kept so later rounds and restarts
// skip its identity reads. Spec is set once the factory registration and pool
// metadata checked out; that identity never changes. A pool with Until above
// the current block is skipped until then: it was rejected, inactive or above
// the fee cap.
type Verdict struct {
	Key   domain.PoolKey
	Spec  *bootstrap.Spec `json:",omitempty"`
	Until uint64          `json:",omitempty"`
}

// Cooldown is an ActiveProbe negative entry: Key is not probed again before Until.
type Cooldown struct {
	Key   domain.PoolKey
	Until uint64
}

// V4ScanState persists one manager's in-progress Initialize backfill so a
// restart resumes at End instead of re-walking from the chain head, which
// takes about an hour. End is the next, lower block the scan
// continues down to; Pending are the pool IDs still unresolved, whose
// Initialize therefore lies at or below End.
type V4ScanState struct {
	Manager domain.Address
	End     uint64
	Pending []domain.PoolKey
}

type indexFile struct {
	Version  int
	ChainID  uint64
	Identity domain.Hash
	Anchor   domain.BlockRef
	Ranges   map[string]indexRange
	Records  []Creation
	Activity []ActivityRecord
	Report   []Assessment
	Verdicts []Verdict     `json:",omitempty"`
	Probes   []Cooldown    `json:",omitempty"`
	V4Scans  []V4ScanState `json:",omitempty"`
	Judged   domain.Hash
}
type indexRange struct {
	From, To uint64
	Logs     []rpc.Log
}

func (s *CreationIndex) Load() (domain.BlockRef, error) {
	info, err := os.Stat(s.Path)
	if os.IsNotExist(err) {
		return domain.BlockRef{}, nil
	}
	if err != nil {
		return domain.BlockRef{}, err
	}
	if info.Size() > 128<<20 {
		return domain.BlockRef{}, fmt.Errorf("discovery index exceeds 128 MiB")
	}
	raw, err := os.ReadFile(s.Path)
	if os.IsNotExist(err) {
		return domain.BlockRef{}, nil
	}
	if err != nil {
		return domain.BlockRef{}, err
	}
	var file indexFile
	if err := json.Unmarshal(raw, &file); err != nil {
		return domain.BlockRef{}, fmt.Errorf("discovery index: %w", err)
	}
	if file.Version != 1 || file.ChainID != s.ChainID {
		return domain.BlockRef{}, fmt.Errorf("discovery index identity differs from configuration")
	}
	if file.Identity != s.Identity {
		// Emitters or deployments changed, e.g. a new protocol source. The index
		// is a rebuildable cache: start empty and let the next Save replace it.
		slog.Warn("discovery index belongs to another emitter set; rebuilding", "path", s.Path)
		return domain.BlockRef{}, nil
	}
	if len(file.Records) > 100000 || len(file.Report) > 100000 || len(file.Verdicts) > 100000 || len(file.Probes) > 100000 {
		return domain.BlockRef{}, fmt.Errorf("discovery index exceeds record limit")
	}
	scanPending := 0
	for _, scan := range file.V4Scans {
		scanPending += len(scan.Pending)
	}
	if len(file.V4Scans) > 1024 || scanPending > 100000 {
		return domain.BlockRef{}, fmt.Errorf("discovery index exceeds V4 scan limit")
	}
	for i, scan := range file.V4Scans {
		// A scan resumes downward from End; a cursor above the anchor, or with
		// nothing left to find, cannot be resumed and is dropped as stale.
		if scan.End > file.Anchor.Number || scan.End == 0 || len(scan.Pending) == 0 {
			file.V4Scans[i].Pending = nil
		}
	}
	file.V4Scans = slices.DeleteFunc(file.V4Scans, func(s V4ScanState) bool { return len(s.Pending) == 0 })
	for _, row := range file.Records {
		if row.Spec.Address == (domain.Address{}) || row.Block > file.Anchor.Number || row.Source != 0 && (!row.Source.Valid() || row.Emitter == (domain.Address{}) || row.Source.PoolKind() != row.Spec.Kind) {
			return domain.BlockRef{}, fmt.Errorf("discovery index has invalid pool metadata")
		}
	}
	ranges := make(map[string]logWindow, len(file.Ranges))
	total := 0
	for key, r := range file.Ranges {
		if r.From > r.To || r.To > file.Anchor.Number {
			return domain.BlockRef{}, fmt.Errorf("discovery index has invalid range")
		}
		total += len(r.Logs)
		if total > 100000 {
			return domain.BlockRef{}, fmt.Errorf("discovery index exceeds log limit")
		}
		for _, log := range r.Logs {
			if log.Removed || log.BlockNumber < r.From || log.BlockNumber > r.To {
				return domain.BlockRef{}, fmt.Errorf("discovery index has invalid log")
			}
		}
		ranges[key] = logWindow{from: r.From, to: r.To, logs: r.Logs}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchor, s.ranges, s.records = file.Anchor, ranges, file.Records
	s.activity, s.report = file.Activity, file.Report
	s.verdicts, s.probes, s.judged = file.Verdicts, file.Probes, file.Judged
	s.v4scans = file.V4Scans
	return file.Anchor, nil
}
func (s *CreationIndex) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.anchor = domain.BlockRef{}
	s.ranges = nil
	s.records = nil
	s.activity = nil
	s.report = nil
	s.verdicts = nil
	s.probes = nil
	s.judged = domain.Hash{}
	s.epoch++
}

func (s *CreationIndex) GetLogs(ctx context.Context, filter rpc.LogFilter) ([]rpc.Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filter.FromBlock > filter.ToBlock {
		return nil, fmt.Errorf("discovery index: inverted range")
	}
	raw, err := json.Marshal([]any{filter.Addresses, filter.Topics})
	if err != nil {
		return nil, err
	}
	key := string(raw)
	s.mu.Lock()
	cached, ok := s.ranges[key]
	epoch := s.epoch
	s.mu.Unlock()
	if ok && filter.FromBlock >= cached.from && filter.ToBlock <= cached.to {
		return selectWindowLogs(cached.logs, filter.FromBlock, filter.ToBlock), nil
	}
	query := filter
	contiguous := ok && filter.FromBlock >= cached.from && (filter.FromBlock <= cached.to || filter.FromBlock-cached.to == 1)
	if contiguous {
		query.FromBlock = cached.to + 1
	}
	fresh, err := s.Source.GetLogs(ctx, query)
	if err != nil {
		return nil, err
	}
	combined := selectWindowLogs(fresh, query.FromBlock, query.ToBlock)
	from := query.FromBlock
	if contiguous {
		from = cached.from
		combined = append(selectWindowLogs(cached.logs, cached.from, cached.to), combined...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch != s.epoch {
		return nil, fmt.Errorf("discovery index generation changed during scan")
	}
	total := len(combined)
	for k, r := range s.ranges {
		if k != key {
			total += len(r.logs)
		}
	}
	if total > 100000 {
		return nil, fmt.Errorf("discovery index exceeds 100000 creation logs; narrow scan range")
	}
	if s.ranges == nil {
		s.ranges = map[string]logWindow{}
	}
	s.ranges[key] = logWindow{from: from, to: filter.ToBlock, logs: combined}
	return selectWindowLogs(combined, filter.FromBlock, filter.ToBlock), nil
}

// Save publishes only ranges completed at or below the verified anchor.
func (s *CreationIndex) Save(ctx context.Context, anchor domain.BlockRef) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	file := indexFile{Version: 1, ChainID: s.ChainID, Identity: s.Identity, Anchor: anchor, Ranges: map[string]indexRange{}, Records: s.records, Activity: s.activity, Report: s.report, Verdicts: s.verdicts, Probes: s.probes, V4Scans: s.v4scans, Judged: s.judged}
	for key, r := range s.ranges {
		if r.to > anchor.Number {
			s.mu.Unlock()
			return fmt.Errorf("discovery index exceeds verified anchor")
		}
		file.Ranges[key] = indexRange{r.from, r.to, r.logs}
	}
	epoch := s.epoch
	s.mu.Unlock()
	raw, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if len(raw) > 128<<20 {
		return fmt.Errorf("discovery index exceeds 128 MiB")
	}
	if err := writeIndex(s.Path, raw); err != nil {
		return err
	}
	s.mu.Lock()
	if epoch == s.epoch {
		s.anchor = anchor
	}
	s.mu.Unlock()
	return nil
}
func writeIndex(path string, raw []byte) (err error) {
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0700); err != nil {
		return err
	}
	file, err := os.CreateTemp(parent, ".pool-index-*")
	if err != nil {
		return err
	}
	defer func() { file.Close(); os.Remove(file.Name()) }()
	if err = file.Chmod(0600); err != nil {
		return err
	}
	if _, err = file.Write(raw); err != nil {
		return err
	}
	if err = file.Sync(); err != nil {
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	if err = os.Rename(file.Name(), path); err != nil {
		return err
	}
	directory, err := os.Open(parent)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (s *CreationIndex) Records() []Creation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Creation(nil), s.records...)
}
func (s *CreationIndex) Remember(records []Creation) {
	s.mu.Lock()
	defer s.mu.Unlock()
	positions := map[domain.PoolKey]int{}
	for i, r := range s.records {
		positions[r.Spec.PoolKey()] = i
	}
	for _, r := range records {
		key := r.Spec.PoolKey()
		if i, seen := positions[key]; seen {
			if r.Verified {
				s.records[i] = r
			}
			continue
		}
		if len(s.records) >= 100000 {
			break
		}
		positions[key] = len(s.records)
		s.records = append(s.records, r)
	}
}

// Verdicts returns the saved verification outcomes and probe cooldowns, or
// none when they were made under another configuration (VerdictIdentity).
func (s *CreationIndex) Verdicts(identity domain.Hash) ([]Verdict, []Cooldown) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if identity != s.judged {
		return nil, nil
	}
	return append([]Verdict(nil), s.verdicts...), append([]Cooldown(nil), s.probes...)
}

// V4Scans returns the saved in-progress Initialize backfills, or none when
// they were made under another configuration (VerdictIdentity).
func (s *CreationIndex) V4Scans(identity domain.Hash) []V4ScanState {
	s.mu.Lock()
	defer s.mu.Unlock()
	if identity != s.judged {
		return nil
	}
	return append([]V4ScanState(nil), s.v4scans...)
}

// SetVerdicts replaces the outcomes the next Save publishes.
func (s *CreationIndex) SetVerdicts(identity domain.Hash, verdicts []Verdict, probes []Cooldown, v4scans []V4ScanState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.judged = identity
	s.verdicts = append([]Verdict(nil), verdicts...)
	s.probes = append([]Cooldown(nil), probes...)
	s.v4scans = append([]V4ScanState(nil), v4scans...)
}

func (s *CreationIndex) Activity() []ActivityRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ActivityRecord(nil), s.activity...)
}
func (s *CreationIndex) SetMarketReport(activity []ActivityRecord, report []Assessment) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.activity = append([]ActivityRecord(nil), activity...)
	s.report = append([]Assessment(nil), report...)
}
