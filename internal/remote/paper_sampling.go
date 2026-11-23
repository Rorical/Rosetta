package remote

import (
	"encoding/json"
	"fmt"
	"math/big"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pipeline"
)

const paperStratumLimit = 256

type paperStratum struct {
	Asset       domain.Address `json:"asset"`
	Hops        int            `json:"hops"`
	Protocols   string         `json:"protocol_counts"`
	GrossDigits int            `json:"gross_raw_digits"`
	Supported   bool           `json:"executor_pool_kinds_supported"`
}
type paperSelection struct {
	QuoteStage     arb.QuoteStage `json:"quote_stage,omitempty"`
	WindowWaitNS   int64          `json:"sampling_wait_ns,omitempty"`
	Mode           string         `json:"mode"`
	Stratum        paperStratum   `json:"stratum"`
	NativeGross    *big.Int       `json:"quoted_gross_native_wei,omitempty"`
	ValuationBasis string         `json:"valuation_basis"`
}
type paperSkippedBucket struct {
	Stratum   paperStratum        `json:"stratum"`
	Reasons   map[string]uint64   `json:"reasons"`
	MaxReason string              `json:"max_gross_reason"`
	MaxInput  *big.Int            `json:"max_gross_input,omitempty"`
	MaxGross  *big.Int            `json:"max_quoted_gross_raw"`
	MaxCycle  graph.Cycle         `json:"max_gross_cycle"`
	Version   domain.StateVersion `json:"max_gross_version"`
	AtNS      int64               `json:"max_gross_observed_at_ns"`
}
type paperSamplingSummary struct {
	Type     string               `json:"type"`
	Basis    string               `json:"basis"`
	AtNS     int64                `json:"at_ns"`
	Buckets  []paperSkippedBucket `json:"buckets"`
	Overflow uint64               `json:"overflow_observations"`
}

// Online owner chooses; the serial worker drains bounded skip summaries.
// Counts are quote observations, including repeated quotes across frames.
type paperSampler struct {
	mu       sync.Mutex
	skipped  map[paperStratum]*paperSkippedBucket
	overflow uint64
	selected map[paperStratum]uint64
	routes   map[domain.Hash]uint64
	rounds   uint64
}

func paperStratumFor(o arb.Opportunity) paperStratum {
	kinds := map[domain.PoolKind]int{}
	supported := true
	for _, key := range o.Cycle.Pools {
		kinds[key.Kind]++
		if err := execution.CheckExecutorPool(key); err != nil {
			supported = false
		}
	}
	order := make([]int, 0, len(kinds))
	for kind := range kinds {
		order = append(order, int(kind))
	}
	sort.Ints(order)
	var parts []string
	for _, kind := range order {
		parts = append(parts, fmt.Sprintf("%d:%d", kind, kinds[domain.PoolKind(kind)]))
	}
	return paperStratum{Asset: o.Cycle.Start, Hops: len(o.Cycle.Pools), Protocols: strings.Join(parts, ","), GrossDigits: len(o.Net.String()), Supported: supported}
}
func (s *paperSampler) skip(o arb.Opportunity, version domain.StateVersion, now time.Time, reason string) {
	if o.Net == nil || o.Net.Sign() <= 0 {
		return
	}
	key := paperStratumFor(o)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.skipped == nil {
		s.skipped = map[paperStratum]*paperSkippedBucket{}
	}
	b := s.skipped[key]
	if b == nil {
		if len(s.skipped) == paperStratumLimit {
			s.overflow++
			return
		}
		b = &paperSkippedBucket{Stratum: key, Reasons: map[string]uint64{}}
		s.skipped[key] = b
	}
	b.Reasons[reason]++
	if b.MaxGross == nil || o.Net.Cmp(b.MaxGross) > 0 {
		b.MaxReason = reason
		if o.Input != nil {
			b.MaxInput = new(big.Int).Set(o.Input)
		} else {
			b.MaxInput = nil
		}
		b.MaxGross = new(big.Int).Set(o.Net)
		b.MaxCycle = o.Cycle
		b.MaxCycle.Pools = append([]domain.PoolKey(nil), o.Cycle.Pools...)
		b.Version = version
		b.AtNS = now.UnixNano()
	}
}
func (s *paperSampler) drain() *paperSamplingSummary {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.skipped) == 0 && s.overflow == 0 {
		return nil
	}
	out := &paperSamplingSummary{Type: "sampling_summary", Basis: "executor_simulation", AtNS: time.Now().UnixNano(), Overflow: s.overflow}
	for _, b := range s.skipped {
		out.Buckets = append(out.Buckets, *b)
	}
	sort.Slice(out.Buckets, func(i, j int) bool {
		a, _ := json.Marshal(out.Buckets[i].Stratum)
		b, _ := json.Marshal(out.Buckets[j].Stratum)
		return string(a) < string(b)
	})
	s.skipped = nil
	s.overflow = 0
	return out
}
func paperRouteIdentity(o arb.Opportunity) domain.Hash {
	// Input is deliberately excluded: repeated sizing of one route must not
	// exhaust the diversity slots. State identity remains in the sample itself.
	raw, _ := json.Marshal(o.Cycle)
	return digest(raw)
}
func (s *paperSampler) chooseCandidate(event arb.SearchEvent, research *pipeline.Research, wrapped domain.Address, accept func(int, *paperSelection, arb.Opportunity) bool) (int, *paperSelection) {
	mode := "native_gross_priority"
	if s.rounds%2 == 1 {
		mode = "stratified"
	}
	return s.choose(event, research, wrapped, mode, accept)
}

func (s *paperSampler) choose(event arb.SearchEvent, research *pipeline.Research, wrapped domain.Address, mode string, accept func(int, *paperSelection, arb.Opportunity) bool) (int, *paperSelection) {
	if s.selected == nil {
		s.selected = map[paperStratum]uint64{}
		s.routes = map[domain.Hash]uint64{}
	}
	best := -1
	var chosen *paperSelection
	for i, o := range event.Result.Opportunities {
		if o.Net == nil || o.Net.Sign() <= 0 {
			continue
		}
		key := paperStratumFor(o)
		if !key.Supported {
			continue
		}
		native, basis, _ := research.NativeValue(event.Job.View, o.Cycle, wrapped, o.Net)
		candidate := &paperSelection{QuoteStage: event.Stage, Mode: mode, Stratum: key, NativeGross: native, ValuationBasis: basis}
		if accept != nil && !accept(i, candidate, o) {
			continue
		}
		better := best < 0
		if !better {
			better = s.prefers(candidate, o, chosen, event.Result.Opportunities[best])
		}
		if better {
			best, chosen = i, candidate
		}
	}
	return best, chosen
}
func (s *paperSampler) admit(chosen *paperSelection, o arb.Opportunity) {
	if len(s.selected) >= paperStratumLimit {
		clear(s.selected)
	}
	if len(s.routes) >= 4096 {
		clear(s.routes)
	}
	s.selected[chosen.Stratum]++
	s.routes[paperRouteIdentity(o)]++
	if chosen.Mode != "high_gross_priority" {
		s.rounds++
	}
}
func (s *paperSampler) prefers(candidate *paperSelection, o arb.Opportunity, chosen *paperSelection, previous arb.Opportunity) bool {
	key, old := candidate.Stratum, chosen.Stratum
	route, prior := paperRouteIdentity(o), paperRouteIdentity(previous)
	if candidate.Mode == "stratified" && s.selected[key] != s.selected[old] {
		return s.selected[key] < s.selected[old]
	}
	if candidate.Mode == "stratified" && s.routes[route] != s.routes[prior] {
		return s.routes[route] < s.routes[prior]
	}
	if candidate.NativeGross != nil && chosen.NativeGross == nil {
		return true
	}
	if candidate.NativeGross == nil && chosen.NativeGross != nil {
		return false
	}
	if candidate.NativeGross != nil && candidate.NativeGross.Cmp(chosen.NativeGross) != 0 {
		return candidate.NativeGross.Cmp(chosen.NativeGross) > 0
	}
	if key.Asset == old.Asset && o.Net.Cmp(previous.Net) != 0 {
		return o.Net.Cmp(previous.Net) > 0
	}
	return route.Hex() < prior.Hex()
}
