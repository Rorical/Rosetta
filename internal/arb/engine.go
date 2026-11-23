package arb

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"math/big"
	"reflect"
	"slices"
	"sync/atomic"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Engine evaluates cycles from one table at a fixed notional and candidate cap,
// refines two-pool cycles with the constant-liquidity closed form, and applies
// a fixed gas cost. The candidate cap is dynamic: SetK may change it while
// evaluations run. Engine is safe for concurrent use when its Quoter is.
type Engine struct {
	table       CycleTable
	compiled    *compiledPaths
	exploration *explorationConfig
	notional    NotionalFunc
	k           atomic.Int64
	quoter      Quoter
	gasCost     *big.Int
	gasToken    domain.Address
	phantoms    phantomPools
}

var (
	_ CycleTable = (*graph.Table)(nil)
	_ CycleTable = (*graph.DeepTable)(nil)
	_ CycleTable = (*graph.MergedTable)(nil)
)

// New returns an engine that evaluates at most k cycles per dirty pool.
// Quoter prices each leg; callers inject the protocol adapters they need.
//
// A NotionalFunc cannot be range-checked before it is called, so Evaluate
// skips cycles whose notional is nil or non-positive.
func New(table CycleTable, notional NotionalFunc, k int, quoter Quoter) (*Engine, error) {
	if isNilTable(table) {
		return nil, fmt.Errorf("%w: nil table", ErrInvalidConfig)
	}
	if notional == nil {
		return nil, fmt.Errorf("%w: nil notional", ErrInvalidConfig)
	}
	if k <= 0 {
		return nil, fmt.Errorf("%w: k must be positive, got %d", ErrInvalidConfig, k)
	}
	if quoter == nil {
		return nil, fmt.Errorf("%w: nil quoter", ErrInvalidConfig)
	}
	engine := &Engine{
		table:    table,
		notional: notional,
		quoter:   quoter,
		gasCost:  new(big.Int),
	}
	engine.k.Store(int64(k))
	return engine, nil
}

// SetK changes the per-dirty-pool candidate cap used by later evaluations. A
// non-positive value is rejected so the engine cannot silently stop evaluating.
func (e *Engine) SetK(k int) error {
	if k <= 0 {
		return fmt.Errorf("%w: k must be positive, got %d", ErrInvalidConfig, k)
	}
	e.k.Store(int64(k))
	return nil
}

// K returns the current per-dirty-pool candidate cap.
func (e *Engine) K() int {
	return int(e.k.Load())
}

// isNilTable reports whether table is nil or holds a typed nil pointer. A
// typed nil passes an interface comparison but fails on the first method call,
// so New rejects it up front.
func isNilTable(table CycleTable) bool {
	if table == nil {
		return true
	}
	value := reflect.ValueOf(table)
	return value.Kind() == reflect.Pointer && value.IsNil()
}

// WithGas returns a copy of e that reports cfg as the gas cost of every
// opportunity. The receiver is unchanged, so engines with different gas
// assumptions can share a table and quoter. The copy takes the current
// candidate cap. A nil or negative Price is treated as zero: a negative gas
// cost would inflate profit. cfg.Token selects the denomination; a cost is
// only subtracted from cycles that start in that token.
func (e *Engine) WithGas(cfg GasConfig) *Engine {
	copied := &Engine{
		table:       e.table,
		compiled:    e.compiled,
		exploration: e.exploration,
		notional:    e.notional,
		quoter:      e.quoter,
		gasCost:     new(big.Int),
		gasToken:    cfg.Token,
	}
	copied.k.Store(e.k.Load())
	if cfg.Price != nil && cfg.Price.Sign() > 0 {
		copied.gasCost.Mul(new(big.Int).SetUint64(cfg.Units), cfg.Price)
	}
	return copied
}

// Evaluate quotes every candidate cycle reachable from dirty. Cycles that
// cannot be quoted, including cycles whose pools are absent from view, are
// omitted. Results are sorted by pre-gas net descending, then by start token
// and pool sequence, so equal inputs produce equal output order.
func (e *Engine) Evaluate(ctx context.Context, view View, dirty []domain.PoolKey) ([]Opportunity, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if view == nil {
		return nil, ErrNilView
	}

	// Read K once so one evaluation uses one consistent cap even if SetK
	// changes it concurrently.
	candidates := e.table.Candidates(dirty, e.K())
	if len(candidates) == 0 {
		return nil, nil
	}

	opportunities := make([]Opportunity, 0, len(candidates))
	for _, index := range candidates {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		opportunity, quoted, err := e.evaluateCycle(ctx, view, e.table.Cycle(index))
		if err != nil {
			return nil, err
		}
		if quoted {
			opportunities = append(opportunities, opportunity)
		}
	}
	sortOpportunities(opportunities)
	return opportunities, nil
}

// evaluateCycle prices one cycle in traversal order. It quotes the notional
// input first, then, for a two-pool cycle whose constant-liquidity closed form
// is profitable, quotes the optimal input and keeps the larger pre-gas net. It
// returns quoted=false when the cycle cannot be fully priced; err is non-nil
// only when ctx is done.
func (e *Engine) evaluateCycle(ctx context.Context, view View, cycle graph.Cycle) (Opportunity, bool, error) {
	notional := e.notional(cycle.Start)
	if notional == nil || notional.Sign() <= 0 {
		return Opportunity{}, false, nil
	}

	opportunity, quoted, err := e.quote(ctx, view, cycle, new(big.Int).Set(notional))
	if err != nil || !quoted {
		return Opportunity{}, false, err
	}
	opportunity, err = e.refine(ctx, view, cycle, opportunity)
	if err != nil {
		return Opportunity{}, false, err
	}

	opportunity.GasCost = new(big.Int).Set(e.gasCost)
	switch {
	case e.gasCost.Sign() == 0:
		// No gas charged: the two nets are equal and comparable regardless of
		// the configured token.
		opportunity.NetAfterGas = new(big.Int).Set(opportunity.Net)
		opportunity.GasComparable = true
	case cycle.Start == e.gasToken:
		opportunity.NetAfterGas = new(big.Int).Sub(opportunity.Net, e.gasCost)
		opportunity.GasComparable = true
	default:
		// Net and GasCost are in different tokens, so subtracting would mix
		// denominations. Report the pre-gas net instead.
		opportunity.NetAfterGas = new(big.Int).Set(opportunity.Net)
		opportunity.GasComparable = false
	}
	return opportunity, true, nil
}

// refine replaces best with a quote at the closed-form optimal input when the
// two-leg model applies and its quoted net beats the fixed-notional quote.
// Longer cycles and cycles whose reserves cannot be read keep best unchanged.
// The closed form is a sizing hint, so an unusable result is not an error.
func (e *Engine) refine(ctx context.Context, view View, cycle graph.Cycle, best Opportunity) (Opportunity, error) {
	first, second, ok := closedFormLegs(view, cycle)
	if !ok {
		return best, nil
	}
	dStar, _, _, profitable, err := math.ClosedFormTwoLeg(first, second)
	if err != nil || !profitable || dStar.Cmp(best.Input) == 0 {
		return best, nil
	}

	candidate, quoted, err := e.quote(ctx, view, cycle, dStar)
	if err != nil {
		return Opportunity{}, err
	}
	if quoted && candidate.Net.Cmp(best.Net) > 0 {
		return candidate, nil
	}
	return best, nil
}

// needsDeep reports whether pricing a pool of kind needs tick-level state.
// V3, Camelot V3, Slipstream, and V4 are concentrated-liquidity pools, so all
// four quote from tick state; a pool without it is skipped rather than priced
// as if no tick were initialized.
func needsDeep(kind domain.PoolKind) bool {
	switch kind {
	case domain.PoolKindV3, domain.PoolKindCamelotV3, domain.PoolKindSlipstream, domain.PoolKindV4, domain.PoolKindSolidlyV3:
		return true
	default:
		return false
	}
}

// otherToken returns the token paired with current in state.
func otherToken(state pool.State, current domain.Address) (domain.Address, bool) {
	switch current {
	case state.Token0:
		return state.Token1, true
	case state.Token1:
		return state.Token0, true
	default:
		return domain.Address{}, false
	}
}

// sortOpportunities orders by pre-gas net descending, then start token, then
// pool sequence. Gas is uniform within one evaluation, so the same order holds
// after gas. The final two keys make the order total for equal nets.
func sortOpportunities(opportunities []Opportunity) {
	slices.SortFunc(opportunities, compareOpportunities)
}

func compareOpportunities(a, b Opportunity) int {
	if diff := b.Net.Cmp(a.Net); diff != 0 {
		return diff
	}
	if diff := bytes.Compare(a.Cycle.Start[:], b.Cycle.Start[:]); diff != 0 {
		return diff
	}
	return comparePoolSequences(a.Cycle.Pools, b.Cycle.Pools)
}

func comparePoolSequences(a, b []domain.PoolKey) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if diff := domain.ComparePoolKeys(a[i], b[i]); diff != 0 {
			return diff
		}
	}
	return cmp.Compare(len(a), len(b))
}

// TableHolds reports whether this engine's precomputed table holds cycle in
// any rotation; routes it does not hold came from background exploration.
// The table stores one token rotation per route, so each start along the
// route is tried with token order read from view.
func (e *Engine) TableHolds(view View, cycle graph.Cycle) bool {
	table, ok := e.table.(*graph.Table)
	if !ok || len(cycle.Pools) == 0 {
		return false
	}
	tokens := make([]domain.Address, len(cycle.Pools))
	current := cycle.Start
	for i, key := range cycle.Pools {
		tokens[i] = current
		p, ok := view.Pool(key)
		if !ok {
			return false
		}
		switch current {
		case p.Token0:
			current = p.Token1
		case p.Token1:
			current = p.Token0
		default:
			return false
		}
	}
	rotated := make([]domain.PoolKey, len(cycle.Pools))
	for offset := range cycle.Pools {
		copy(rotated, cycle.Pools[offset:])
		copy(rotated[len(cycle.Pools)-offset:], cycle.Pools[:offset])
		if table.ContainsCycle(graph.Cycle{Start: tokens[offset], Pools: rotated, Len: len(rotated)}) {
			return true
		}
	}
	return false
}
