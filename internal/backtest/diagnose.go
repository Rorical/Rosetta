package backtest

import (
	"context"
	"errors"
	"math/big"
	"slices"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	mathutil "github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

type KnownRoute struct {
	ID        string           `json:"id"`
	BlockHash domain.Hash      `json:"block_hash"`
	BeforeTx  uint64           `json:"before_tx"`
	Cycle     graph.Cycle      `json:"cycle"`
	Input     *big.Int         `json:"input"`
	Tokens    []domain.Address `json:"tokens"`
}
type Diagnostic struct {
	ID             string           `json:"id"`
	Reasons        []string         `json:"reasons"`
	MissingPools   []domain.PoolKey `json:"missing_pools,omitempty"`
	MissingTokens  []domain.Address `json:"missing_tokens,omitempty"`
	Gross          *big.Int         `json:"gross,omitempty"`
	CostAdjusted   *big.Int         `json:"cost_adjusted,omitempty"`
	CostBasis      string           `json:"cost_basis"`
	QuoteError     string           `json:"quote_error,omitempty"`
	Found          bool             `json:"found"`
	TableTruncated bool             `json:"table_truncated"`
}
type DiagnosticOptions struct {
	Diverse      bool
	Model        func(pool.State) (pool.RankingModel, error)
	Depth        int
	Truncated    bool
	ActiveTokens map[domain.Address]bool
	Dirty        []domain.PoolKey
	Bounds       func(domain.Address) (arb.InputBounds, bool)
	SwapFee      func(pool.State, domain.Address) (uint32, error)
	Costs        func(domain.Address, *big.Int, *big.Int) (*big.Int, string)
}

// Diagnose compares one historical path against actual coverage, enumeration,
// ranking and exact quote stages. It never infers realized profit from Swap logs.
func Diagnose(ctx context.Context, view arb.View, table arb.CycleTable, engine *arb.Engine, known KnownRoute, options DiagnosticOptions) (Diagnostic, error) {
	out := Diagnostic{ID: known.ID, CostBasis: "unknown", TableTruncated: options.Truncated}
	add := func(reason string) {
		if !slices.Contains(out.Reasons, reason) {
			out.Reasons = append(out.Reasons, reason)
		}
	}
	for _, key := range known.Cycle.Pools {
		if _, ok := view.Pool(key); !ok {
			out.MissingPools = append(out.MissingPools, key)
		}
	}
	if len(out.MissingPools) > 0 {
		add("missing_pool")
	}
	if options.ActiveTokens != nil {
		for _, token := range known.Tokens {
			if !options.ActiveTokens[token] {
				out.MissingTokens = append(out.MissingTokens, token)
			}
		}
		if len(out.MissingTokens) > 0 {
			add("missing_token")
		}
	}
	if len(known.Cycle.Pools) > options.Depth {
		add("depth_limit")
	}
	bounds, funded := options.Bounds(known.Cycle.Start)
	if !funded {
		add("unfunded_start")
	} else if known.Input == nil || known.Input.Cmp(bounds.Min) < 0 || known.Input.Cmp(bounds.Max) > 0 {
		add("input_outside_bounds")
	}
	if len(out.MissingPools) > 0 || len(out.MissingTokens) > 0 {
		return out, nil
	}
	tokens := func(key domain.PoolKey) (domain.Address, domain.Address, bool) {
		p, ok := view.Pool(key)
		return p.Token0, p.Token1, ok
	}
	present := false
	for i := 0; i < table.Len(); i++ {
		if err := ctx.Err(); err != nil {
			return out, err
		}
		if graph.SameDirectedCycle(table.Cycle(i), known.Cycle, tokens) {
			present = true
			break
		}
	}
	if !present {
		if options.Truncated {
			add("table_truncated")
		} else {
			add("path_not_enumerated")
		}
	}
	quote, err := engine.QuoteCycle(ctx, view, known.Cycle, known.Input)
	if err != nil {
		if ctx.Err() != nil {
			return out, ctx.Err()
		}
		out.QuoteError = err.Error()
		switch {
		case errors.Is(err, domain.ErrUnsupported):
			add("unsupported_protocol")
		case errors.Is(err, arb.ErrMissingDeep), errors.Is(err, mathutil.ErrMissingTick), errors.Is(err, mathutil.ErrOutOfWindow):
			add("missing_ticks")
		default:
			add("quote_failed")
		}
		return out, nil
	}
	out.Gross = quote.Net
	if quote.Net.Sign() <= 0 {
		add("no_gross_profit_at_recorded_input")
	}
	if options.Costs != nil {
		out.CostAdjusted, out.CostBasis = options.Costs(known.Cycle.Start, known.Input, quote.Net)
		if out.CostAdjusted != nil && out.CostAdjusted.Sign() <= 0 {
			add("costs_exceed_profit")
		}
	}
	if !funded || !present {
		return out, nil
	}
	selection, err := engine.Rank(ctx, view, options.Dirty, arb.RankedOptions{Diverse: options.Diverse, Model: options.Model, Bounds: options.Bounds, SwapFee: options.SwapFee})
	if err != nil {
		return out, err
	}
	selected := false
	for _, cycle := range selection.Cycles() {
		if graph.SameDirectedCycle(cycle, known.Cycle, tokens) {
			selected = true
			break
		}
	}
	if !selected {
		overlap := false
		for _, key := range known.Cycle.Pools {
			if slices.Contains(options.Dirty, key) {
				overlap = true
				break
			}
		}
		if overlap {
			add("coarse_ranking_limit")
		} else {
			add("path_not_dirty")
		}
		return out, nil
	}
	result, err := engine.Optimize(ctx, view, selection)
	if err != nil {
		return out, err
	}
	for _, opportunity := range result.Opportunities {
		if graph.SameDirectedCycle(opportunity.Cycle, known.Cycle, tokens) {
			out.Found = true
			break
		}
	}
	if !out.Found {
		add("amount_search_or_exact_quote")
	}
	return out, nil
}
