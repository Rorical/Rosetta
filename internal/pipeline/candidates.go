package pipeline

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

type Encoder interface{ Encode(any) error }

// CandidateWriter emits unconfirmed candidates. It never upgrades a quote to a
// ready execution plan; only a backend's complete confirmation can do that.
type CandidateWriter struct {
	Encoder   Encoder
	Economics []ResearchEconomics
}

func (w CandidateWriter) Consume(_ context.Context, event arb.SearchEvent) error {
	for index, opportunity := range event.Result.Opportunities {
		var economics *ResearchEconomics
		if index < len(w.Economics) {
			economics = &w.Economics[index]
		}
		if err := w.Encoder.Encode(struct {
			Economics    *ResearchEconomics  `json:"economics,omitempty"`
			Timing       QuoteTiming         `json:"timing"`
			QuoteBasis   string              `json:"quote_basis,omitempty"`
			Status       string              `json:"status"`
			Version      domain.StateVersion `json:"version"`
			GraphVersion uint64              `json:"graph_version"`
			Opportunity  arb.Opportunity     `json:"opportunity"`
		}{economics, quoteTiming(event, time.Now()), event.Job.QuoteBasis, "candidate", event.Job.Version, event.Job.GraphVersion, opportunity}); err != nil {
			return fmt.Errorf("write candidate: %w", err)
		}
	}
	return nil
}

// SelectCandidates applies funding admission before the confirmation cap and
// retains all selected legs for the next dirty search. Input order is ranking.
func SelectCandidates(opportunities []arb.Opportunity, limit int, allowed func(domain.Address) bool) ([]arb.Opportunity, []domain.PoolKey) {
	selected := make([]arb.Opportunity, 0, min(len(opportunities), max(0, limit)))
	var retry []domain.PoolKey
	if limit <= 0 {
		return selected, retry
	}
	for _, opportunity := range opportunities {
		if !allowed(opportunity.Cycle.Start) {
			continue
		}
		selected = append(selected, opportunity)
		retry = append(retry, opportunity.Cycle.Pools...)
		if len(selected) == limit {
			break
		}
	}
	slices.SortFunc(retry, domain.ComparePoolKeys)
	return selected, slices.Compact(retry)
}

// ProfitablePlanPools credits positive complete simulations, never realized fills.
func ProfitablePlanPools(plan *execution.ReadyPlan) []domain.PoolKey {
	if plan == nil || plan.Economics.BudgetNetProfit == nil || plan.Economics.BudgetNetProfit.Sign() <= 0 {
		return nil
	}
	keys := make([]domain.PoolKey, 0, len(plan.Route.Steps))
	for _, step := range plan.Route.Steps {
		keys = append(keys, step.Pool)
	}
	slices.SortFunc(keys, domain.ComparePoolKeys)
	return slices.Compact(keys)
}
