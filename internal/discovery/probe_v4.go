package discovery

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv4"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func (p *ActiveProbe) assess(key domain.PoolKey, reason string, options ProbeOptions) {
	p.Report = append(p.Report, Assessment{Pool: key, Reason: reason, Activity: ActivityFor(options.Activity, key)})
}

// v4ScanIDs bounds the pool IDs one Initialize scan carries. Every history
// chunk is one eth_getLogs whose topic filter lists all of them, so more IDs
// cost almost nothing per request, while a scan runs to genesis (about 72
// minutes on Robinhood) before it takes new IDs.
const v4ScanIDs = 128

// Query only observed IDs on configured managers. Successful immutable metadata
// is retained by CreationIndex; negative results have a bounded cooldown.
// At most v4ScanIDs IDs per manager are admitted in a round.
func (p *ActiveProbe) backfillV4(ctx context.Context, logs LogSource, keys []domain.PoolKey, options ProbeOptions) ([]Creation, error) {
	if p.historicalRangeUnsupported {
		for _, key := range keys {
			p.assess(key, "historical_log_range_unsupported", options)
		}
		return nil, nil
	}
	groups := map[domain.Address][]domain.PoolKey{}
	managers := []domain.Address{}
	limit := min(v4ScanIDs, options.Limit)
	for _, key := range keys {
		if options.Seen[key] {
			continue
		}
		configured := false
		for _, emitter := range options.Emitters {
			configured = configured || (emitter.Source == SourceUniswapV4 && emitter.Address == key.Address)
		}
		if !configured {
			p.assess(key, "unconfigured_v4_manager", options)
			continue
		}
		if limit == 0 {
			p.assess(key, "metadata_probe_budget", options)
			continue
		}
		if logs == nil {
			p.assess(key, "v4_initialize_source_unavailable", options)
			continue
		}
		if len(groups[key.Address]) == 0 {
			managers = append(managers, key.Address)
		}
		groups[key.Address] = append(groups[key.Address], key)
		limit--
	}
	var rows []Creation
	for _, manager := range managers {
		scan := p.v4Scans[manager]
		if scan == nil {
			scan = &v4Scan{requested: map[domain.Hash]domain.PoolKey{}, pending: map[domain.Hash]domain.PoolKey{}, end: options.Anchor.Number}
			for _, key := range groups[manager] {
				scan.requested[key.PoolID] = key
				scan.pending[key.PoolID] = key
			}
		}
		found, done, err := scan.advance(ctx, logs, manager, options.HistoryChunk, options.HistoryChunksPerRound)
		if errors.Is(err, ErrHistoryLogsUnavailable) {
			// No cooldown: the history endpoint is retried next round.
			slog.Warn("V4 Initialize lookup unavailable", "manager", manager, "pools", len(scan.pending), "error", err)
			for _, key := range scan.pending {
				p.assess(key, "v4_initialize_source_unavailable", options)
			}
			p.keepScan(manager, scan)
			continue
		}
		if err != nil {
			var reply *rpc.RPCError
			if errors.As(err, &reply) {
				message := strings.ToLower(reply.Message)
				if (reply.Code == -32602 || reply.Code == -32005) && strings.Contains(message, "range") && (strings.Contains(message, "limit") || strings.Contains(message, "exceed") || strings.Contains(message, "maximum")) {
					p.historicalRangeUnsupported = true
					for _, key := range keys {
						p.assess(key, "historical_log_range_unsupported", options)
					}
					return rows, nil
				}
			}
			p.keepScan(manager, scan)
			return nil, fmt.Errorf("discovery V4 Initialize backfill: %w", err)
		}
		rows = append(rows, p.admitInitialize(manager, scan.requested, found, options)...)
		if !done {
			for _, key := range scan.pending {
				p.assess(key, "v4_initialize_scanning", options)
			}
			p.keepScan(manager, scan)
			continue
		}
		delete(p.v4Scans, manager)
		for _, key := range scan.pending {
			p.assess(key, "v4_initialize_not_found", options)
			if len(p.After) < 4096 {
				p.After[key] = options.Anchor.Number + max(1, options.Cooldown)
			}
		}
	}
	return rows, nil
}

// admitInitialize decodes the Initialize logs of the requested pools and
// returns their creations. A pool with an invalid log is assessed and put on
// cooldown; one without a log stays with the caller.
func (p *ActiveProbe) admitInitialize(manager domain.Address, requested map[domain.Hash]domain.PoolKey, found []rpc.Log, options ProbeOptions) []Creation {
	var rows []Creation
	answered := map[domain.PoolKey]bool{}
	for _, log := range found {
		if log.Removed || log.BlockNumber > options.Anchor.Number || log.Address != manager || len(log.Topics) < 2 {
			continue
		}
		key, wanted := requested[log.Topics[1]]
		if !wanted || answered[key] {
			continue
		}
		answered[key] = true
		spec, ok, err := DecodeV4Initialize(log)
		if err != nil || !ok {
			p.assess(key, "invalid_v4_initialize", options)
			continue
		}
		id, err := uniswapv4.PoolID(spec.Token0, spec.Token1, spec.Fee, spec.TickSpacing, spec.Hooks)
		if err != nil || id != key.PoolID {
			p.assess(key, "invalid_v4_pool_id", options)
			continue
		}
		// Keep valid but unsupported metadata in the index so hooks are reported
		// without repeatedly fetching the same historical event.
		rows = append(rows, Creation{Spec: spec, Source: SourceUniswapV4, Emitter: manager, Block: log.BlockNumber})
		options.Seen[spec.PoolKey()] = true
		reason := "metadata_verified"
		if spec.Hooks != (domain.Address{}) {
			reason = "unsupported_v4_hooks"
		}
		p.assess(key, reason, options)
	}
	for _, key := range requested {
		if answered[key] && !options.Seen[key] && len(p.After) < 4096 {
			p.After[key] = options.Anchor.Number + max(1, options.Cooldown)
		}
	}
	return rows
}

func (p *ActiveProbe) keepScan(manager domain.Address, scan *v4Scan) {
	if p.v4Scans == nil {
		p.v4Scans = map[domain.Address]*v4Scan{}
	}
	p.v4Scans[manager] = scan
}

// ScanStates captures the unfinished Initialize backfills for persistence, so
// a restart resumes each at its cursor rather than from the chain head.
func (p *ActiveProbe) ScanStates() []V4ScanState {
	states := make([]V4ScanState, 0, len(p.v4Scans))
	for manager, scan := range p.v4Scans {
		pending := make([]domain.PoolKey, 0, len(scan.pending))
		for _, key := range scan.pending {
			pending = append(pending, key)
		}
		states = append(states, V4ScanState{Manager: manager, End: scan.end, Pending: pending})
	}
	return states
}

// RestoreScans rebuilds the backfills saved by ScanStates. Only the pools
// still pending are carried; found pools are already in the creation index.
func (p *ActiveProbe) RestoreScans(states []V4ScanState) {
	if len(states) == 0 {
		return
	}
	p.v4Scans = make(map[domain.Address]*v4Scan, len(states))
	for _, state := range states {
		if state.End == 0 || len(state.Pending) == 0 {
			continue
		}
		scan := &v4Scan{requested: map[domain.Hash]domain.PoolKey{}, pending: map[domain.Hash]domain.PoolKey{}, end: state.End}
		for _, key := range state.Pending {
			scan.requested[key.PoolID] = key
			scan.pending[key.PoolID] = key
		}
		p.v4Scans[state.Manager] = scan
	}
}

// v4Scan is one manager's exact-ID Initialize lookup, walking back from the
// anchor it started at. A per-round chunk budget leaves it unfinished; the
// next round resumes at end. Blocks after that anchor need no scan: a pool
// created there is inside the creation index's lookback window.
type v4Scan struct {
	requested, pending map[domain.Hash]domain.PoolKey
	end                uint64 // last block of the next range
}

// advance queries up to budget ranges of chunk blocks (zero: no limit; a zero
// chunk is one range from block 0) and reports whether the scan finished:
// every pool found, or block 0 reached.
func (s *v4Scan) advance(ctx context.Context, logs LogSource, manager domain.Address, chunk uint64, budget int) ([]rpc.Log, bool, error) {
	if chunk == 0 {
		chunk = s.end + 1
	}
	// A failed range rolls the scan back to where this call began, so pools
	// found before the failure are found again rather than dropped.
	end, pending := s.end, maps.Clone(s.pending)
	var out []rpc.Log
	for spent := 0; len(s.pending) > 0; spent++ {
		if budget > 0 && spent == budget {
			return out, false, nil
		}
		from := uint64(0)
		if s.end >= chunk {
			from = s.end - chunk + 1
		}
		ids := make([]domain.Hash, 0, len(s.pending))
		for id := range s.pending {
			ids = append(ids, id)
		}
		slices.SortFunc(ids, func(a, b domain.Hash) int { return bytes.Compare(a[:], b[:]) })
		found, err := logs.GetLogs(ctx, rpc.LogFilter{FromBlock: from, ToBlock: s.end, Addresses: []domain.Address{manager}, Topics: [][]domain.Hash{{topicV4Initialize}, ids}})
		if err != nil {
			s.end, s.pending = end, pending
			return nil, false, err
		}
		out = append(out, found...)
		for _, log := range found {
			if len(log.Topics) > 1 {
				delete(s.pending, log.Topics[1])
			}
		}
		if from == 0 {
			return out, true, nil
		}
		s.end = from - 1
	}
	return out, true, nil
}

// DiscoveryReport records one final assessment per pool. Later admission/depth
// results replace metadata-stage status, while unresolved emitters remain visible.
func DiscoveryReport(metadata, admission []Assessment) []Assessment {
	out := append([]Assessment(nil), metadata...)
	positions := map[domain.PoolKey]int{}
	for i, row := range out {
		positions[row.Pool] = i
	}
	for _, row := range admission {
		if i, ok := positions[row.Pool]; ok {
			out[i] = row
		} else {
			positions[row.Pool] = len(out)
			out = append(out, row)
		}
	}
	return out
}
