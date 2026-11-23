package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/Rorical/Rosetta/internal/rpc"
)

// WindowLogs reuses complete creation-log ranges, fetching only a new suffix.
// Its owner must Reset on a chain generation change; mutable height queries
// cannot be cached across reorgs. Window and the entry cap bound retained data.
type WindowLogs struct {
	Source  LogSource
	Window  uint64
	mu      sync.Mutex
	epoch   uint64
	entries map[string]logWindow
}
type logWindow struct {
	from, to uint64
	logs     []rpc.Log
}

// Reset discards height-bound data when the owner changes chain generation.
func (s *WindowLogs) Reset() { s.mu.Lock(); defer s.mu.Unlock(); s.epoch++; s.entries = nil }
func (s *WindowLogs) GetLogs(ctx context.Context, filter rpc.LogFilter) ([]rpc.Log, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if filter.FromBlock > filter.ToBlock {
		return nil, fmt.Errorf("discovery: inverted log window")
	}
	keyBytes, err := json.Marshal([]any{filter.Addresses, filter.Topics})
	if err != nil {
		return nil, err
	}
	key := string(keyBytes)
	s.mu.Lock()
	cached, ok := s.entries[key]
	epoch := s.epoch
	s.mu.Unlock()
	reusable := ok && filter.FromBlock >= cached.from && (filter.FromBlock <= cached.to || filter.FromBlock-cached.to == 1)
	if reusable && filter.ToBlock <= cached.to {
		return selectWindowLogs(cached.logs, filter.FromBlock, filter.ToBlock), nil
	}
	query := filter
	if reusable {
		query.FromBlock = max(query.FromBlock, cached.to+1)
	}
	fresh, err := s.Source.GetLogs(ctx, query)
	if err != nil {
		return nil, err
	}
	combined := fresh
	from := filter.FromBlock
	if reusable {
		combined = append(selectWindowLogs(cached.logs, cached.from, cached.to), fresh...)
		from = cached.from
	}
	result := selectWindowLogs(combined, filter.FromBlock, filter.ToBlock)
	floor := uint64(0)
	if filter.ToBlock > s.Window {
		floor = filter.ToBlock - s.Window
	}
	from = max(from, floor)
	retained := selectWindowLogs(combined, from, filter.ToBlock)
	s.mu.Lock()
	defer s.mu.Unlock()
	if epoch == s.epoch && ctx.Err() == nil {
		if s.entries == nil {
			s.entries = make(map[string]logWindow)
		}
		if len(retained) <= 8192 && (len(s.entries) < 16 || ok) {
			s.entries[key] = logWindow{from: from, to: filter.ToBlock, logs: retained}
		} else {
			delete(s.entries, key)
		}
	}
	return result, nil
}
func selectWindowLogs(logs []rpc.Log, from, to uint64) []rpc.Log {
	var selected []rpc.Log
	for _, log := range logs {
		if log.BlockNumber >= from && log.BlockNumber <= to {
			log.Topics = slices.Clone(log.Topics)
			log.Data = slices.Clone(log.Data)
			selected = append(selected, log)
		}
	}
	return selected
}
