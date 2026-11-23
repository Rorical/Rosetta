package backtest

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

// Block is one replay step. Apply injects the shallow updates and protocol
// events observed at Ref; a nil Apply means the block changed nothing this
// replay tracks. Apply may inspect and mutate state in any way the state store
// allows, but the dirty set it leaves behind is the only work the runner sees.
type Block struct {
	Ref   domain.BlockRef
	Apply func(*state.Store) error
}

// Step is the evaluation result at one block. Opportunities are in engine
// order, which is total for equal inputs, and may be empty.
type Step struct {
	Ref           domain.BlockRef
	Opportunities []arb.Opportunity
}

// Runner replays blocks through one engine. A Run always starts from an empty
// store, so a Runner can be reused and executes the same blocks identically
// regardless of earlier runs. A Runner is safe for concurrent use when its
// Quoter is.
type Runner struct {
	engine *arb.Engine
}

// New returns a Runner that evaluates at most k cycles per dirty pool at the
// notional returned for the cycle's start token. table, quoter, and notional
// are validated once here rather than per block.
func New(table *graph.Table, quoter arb.Quoter, notional arb.NotionalFunc, k int) (*Runner, error) {
	engine, err := arb.New(table, notional, k, quoter)
	if err != nil {
		return nil, fmt.Errorf("backtest: new runner: %w", err)
	}
	return &Runner{engine: engine}, nil
}

// Run replays blocks in order against a fresh store and returns one Step per
// completed block. Each block advances the store head, applies its updates,
// evaluates the resulting dirty set, and clears it so the next block starts
// clean.
//
// On a begin, apply, or evaluate error, Run returns the steps completed before
// the failing block and an error naming that block's number. When ctx is done
// between blocks, Run returns the completed steps and ctx.Err(). The result is
// an empty, non-nil slice when no block completes.
func (r *Runner) Run(ctx context.Context, blocks []Block) ([]Step, error) {
	steps := make([]Step, 0, len(blocks))
	store := state.New()
	view := storeView{store: store}

	for _, block := range blocks {
		if err := ctx.Err(); err != nil {
			return steps, err
		}
		if err := store.BeginBlock(block.Ref); err != nil {
			return steps, fmt.Errorf("backtest: block %d: begin block: %w", block.Ref.Number, err)
		}
		if block.Apply != nil {
			if err := block.Apply(store); err != nil {
				return steps, fmt.Errorf("backtest: block %d: apply: %w", block.Ref.Number, err)
			}
		}

		opportunities, err := r.engine.Evaluate(ctx, view, store.Dirty())
		if err != nil {
			return steps, fmt.Errorf("backtest: block %d: evaluate: %w", block.Ref.Number, err)
		}
		store.ClearDirty()
		steps = append(steps, Step{Ref: block.Ref, Opportunities: opportunities})
	}
	return steps, nil
}

// storeView narrows a Store to the read-only surface the engine may use.
type storeView struct {
	store *state.Store
}

func (v storeView) Pool(key domain.PoolKey) (pool.State, bool) {
	return v.store.Pool(key)
}

func (v storeView) Deep(key domain.PoolKey) (pool.DeepState, bool) {
	return v.store.Deep(key)
}

var _ arb.View = storeView{}
