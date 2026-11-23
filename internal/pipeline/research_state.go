package pipeline

import (
	"context"
	"encoding/json"
	"math/big"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"golang.org/x/crypto/sha3"
)

type RouteQuote func(context.Context, arb.View, graph.Cycle, *big.Int) (arb.Opportunity, error)
type ResearchFrame struct {
	View    arb.View
	Version domain.StateVersion
	Graph   uint64
	At      time.Time
	Quote   RouteQuote
}
type ResearchEvent struct {
	Timing               *QuoteTiming        `json:"timing,omitempty"`
	QuoteBasis           string              `json:"quote_basis,omitempty"`
	ChainID              uint64              `json:"chain_id"`
	Type                 string              `json:"type"`
	ID                   domain.Hash         `json:"id"`
	AtMS                 int64               `json:"at_ms"`
	Version              domain.StateVersion `json:"version"`
	Graph                uint64              `json:"graph"`
	Cycle                *graph.Cycle        `json:"cycle,omitempty"`
	Input                *big.Int            `json:"input,omitempty"`
	Economics            *ResearchEconomics  `json:"economics,omitempty"`
	AgeMS                int64               `json:"age_ms"`
	PositiveSampleSpanMS int64               `json:"positive_sample_span_ms"`
	SearchAgeMS          int64               `json:"search_age_ms,omitempty"`
	Continuous           bool                `json:"continuous_observation"`
	Censored             bool                `json:"censored"`
	Reason               string              `json:"reason,omitempty"`
	Skipped              uint64              `json:"skipped_observations,omitempty"`
	Details              any                 `json:"details,omitempty"`
}
type researchRoute struct {
	quoteBasis          string
	cycle               graph.Cycle
	input               *big.Int
	first, lastPositive time.Time
	version             domain.StateVersion
	graph               uint64
	continuous          bool
}
type researchState struct {
	economics func(arb.View, domain.StateVersion, arb.Opportunity) ResearchEconomics
	policy    ResearchPolicy
	routes    map[domain.Hash]*researchRoute
	emit      func(ResearchEvent) error
}

func routeID(cycle graph.Cycle, input *big.Int, source domain.StateSource) domain.Hash {
	encoded, _ := json.Marshal([]any{cycle, input, source})
	h := sha3.NewLegacyKeccak256()
	h.Write(encoded)
	var hash domain.Hash
	copy(hash[:], h.Sum(nil))
	return hash
}
func (s *researchState) record(kind string, id domain.Hash, row *researchRoute, at time.Time, version domain.StateVersion) ResearchEvent {
	return ResearchEvent{QuoteBasis: row.quoteBasis, Type: kind, ID: id, AtMS: at.UnixMilli(), Version: version, Graph: row.graph, AgeMS: at.Sub(row.first).Milliseconds(), PositiveSampleSpanMS: row.lastPositive.Sub(row.first).Milliseconds(), Continuous: row.continuous}
}
func (s *researchState) discover(event arb.SearchEvent, at time.Time, supplied ...ResearchEconomics) error {
	if s.routes == nil {
		s.routes = map[domain.Hash]*researchRoute{}
	}
	for index, opportunity := range event.Result.Opportunities {
		id := routeID(opportunity.Cycle, opportunity.Input, event.Job.Version.Source)
		if row, exists := s.routes[id]; exists {
			if event.Stage != "" {
				update := s.record("quote_update", id, row, at, event.Job.Version)
				timing := quoteTiming(event, at)
				update.Timing = &timing
				economics := s.candidateEconomics(event, index, opportunity, supplied)
				update.Economics = &economics
				if err := s.emit(update); err != nil {
					return err
				}
			}
			continue
		}
		if len(s.routes) >= s.policy.MaxTracked {
			if err := s.emit(ResearchEvent{Type: "gap", AtMS: at.UnixMilli(), Version: event.Job.Version, Reason: "tracked_route_limit", Skipped: 1, Censored: true}); err != nil {
				return err
			}
			break
		}
		cycle := opportunity.Cycle
		cycle.Pools = slices.Clone(cycle.Pools)
		row := &researchRoute{quoteBasis: event.Job.QuoteBasis, cycle: cycle, input: new(big.Int).Set(opportunity.Input), first: at, lastPositive: at, version: event.Job.Version, graph: event.Job.GraphVersion, continuous: true}
		economics := s.candidateEconomics(event, index, opportunity, supplied)
		output := s.record("candidate", id, row, at, event.Job.Version)
		timing := quoteTiming(event, at)
		output.Timing = &timing
		output.Cycle = &cycle
		output.Input = row.input
		output.Economics = &economics
		if !event.Job.ReceivedAt.IsZero() {
			output.SearchAgeMS = max(0, at.Sub(event.Job.ReceivedAt).Milliseconds())
		}
		if err := s.emit(output); err != nil {
			return err
		}
		if (economics.Net != nil || economics.Calibration != nil) && !economics.Eligible {
			ended := s.record("ended", id, row, at, event.Job.Version)
			ended.Economics = &economics
			ended.Reason = "below_cost_budget"
			if economics.Calibration != nil && economics.Calibration.Reason != "" {
				ended.Reason = economics.Calibration.Reason
				ended.Censored = economics.Calibration.Reason != "below_observed_cost_and_quote_reserve"
			}
			if err := s.emit(ended); err != nil {
				return err
			}
		} else {
			s.routes[id] = row
		}
	}
	return nil
}
func (s *researchState) advance(ctx context.Context, frame ResearchFrame) error {
	ids := make([]domain.Hash, 0, len(s.routes))
	for id := range s.routes {
		ids = append(ids, id)
	}
	slices.SortFunc(ids, func(a, b domain.Hash) int {
		for i := range a {
			if a[i] < b[i] {
				return -1
			}
			if a[i] > b[i] {
				return 1
			}
		}
		return 0
	})
	for _, id := range ids {
		row := s.routes[id]
		if frame.At.Before(row.first) || row.version.Source != frame.Version.Source {
			continue
		}
		reason := ""
		if frame.Version.Source == domain.SourceFast && frame.Version.Epoch != row.version.Epoch {
			reason = "prefix_reset"
		} else if frame.Graph != row.graph {
			reason = "graph_changed"
		} else if frame.At.Sub(row.first).Milliseconds() >= s.policy.MaxAgeMS {
			reason = "observation_timeout"
		} else if frame.Version.Source == domain.SourceFast && frame.Version.Anchor != row.version.Anchor {
			reason = "anchor_changed"
		} else if frame.Version.Source != domain.SourceFast && frame.Version.Anchor.Hash != row.version.Anchor.Hash && frame.Version.Anchor.ParentHash != row.version.Anchor.Hash {
			reason = "canonical_gap_or_reorg"
		}
		if reason != "" {
			event := s.record("ended", id, row, frame.At, frame.Version)
			event.Censored = true
			event.Reason = reason
			if err := s.emit(event); err != nil {
				return err
			}
			delete(s.routes, id)
			continue
		}
		if frame.Version == row.version {
			continue
		}
		quote, err := frame.Quote(ctx, frame.View, row.cycle, row.input)
		output := s.record("recheck", id, row, frame.At, frame.Version)
		if err != nil {
			row.continuous = false
			output.Continuous = false
			output.Reason = err.Error()
			output.Censored = true
			if err := s.emit(output); err != nil {
				return err
			}
			continue
		}
		economics := s.policy.Economics(row.cycle.Start, row.input, quote.Net)
		if s.economics != nil {
			economics = s.economics(frame.View, frame.Version, quote)
		}
		output.Economics = &economics
		positive := quote.Net.Sign() > 0
		if economics.Net != nil || economics.Calibration != nil {
			positive = economics.Eligible
		}
		row.version = frame.Version
		if positive {
			row.lastPositive = frame.At
			output.PositiveSampleSpanMS = frame.At.Sub(row.first).Milliseconds()
		} else {
			output.Type = "ended"
			output.Reason = "profit_disappeared"
			if economics.Calibration != nil && economics.Calibration.Reason != "" {
				output.Reason = economics.Calibration.Reason
				output.Censored = economics.Calibration.Reason != "below_observed_cost_and_quote_reserve"
			}
			delete(s.routes, id)
		}
		if err := s.emit(output); err != nil {
			return err
		}
	}
	return nil
}
func (s *researchState) finish(at time.Time, reason string) error {
	for id, row := range s.routes {
		event := s.record("ended", id, row, at, row.version)
		event.Censored = true
		event.Reason = reason
		if err := s.emit(event); err != nil {
			return err
		}
		delete(s.routes, id)
	}
	return nil
}

func (s *researchState) expire(at time.Time) error {
	for id, row := range s.routes {
		if at.Sub(row.first).Milliseconds() < s.policy.MaxAgeMS {
			continue
		}
		event := s.record("ended", id, row, at, row.version)
		event.Censored = true
		event.Reason = "observation_timeout"
		if err := s.emit(event); err != nil {
			return err
		}
		delete(s.routes, id)
	}
	return nil
}

func (s *researchState) candidateEconomics(event arb.SearchEvent, index int, o arb.Opportunity, supplied []ResearchEconomics) ResearchEconomics {
	if index < len(supplied) {
		return supplied[index]
	}
	if s.economics != nil {
		return s.economics(event.Job.View, event.Job.Version, o)
	}
	return s.policy.Economics(o.Cycle.Start, o.Input, o.Net)
}
