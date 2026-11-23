package simulate

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// AdapterFactory builds the adapter that quotes state's protocol family. It
// must return a non-nil adapter when err is nil. The factory may read state but
// must not modify it.
type AdapterFactory func(state pool.State) (pool.Adapter, error)

// Quoter builds an adapter for each quote and delegates to it, so protocol
// selection stays outside the arbitrage engine. It is safe for concurrent use
// when its factory is.
type Quoter struct {
	factory AdapterFactory
}

// NewQuoter returns a Quoter backed by factory.
func NewQuoter(factory AdapterFactory) (*Quoter, error) {
	if factory == nil {
		return nil, fmt.Errorf("%w: nil adapter factory", ErrInvalidConfig)
	}
	return &Quoter{factory: factory}, nil
}

// QuoteExactIn quotes an exact-input swap against state and deep. It checks ctx
// before building an adapter and wraps factory and adapter errors with the pool
// address.
func (q *Quoter) QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	adapter, err := q.factory(state)
	if err != nil {
		return nil, fmt.Errorf("simulate: build adapter for pool %s: %w", state.Key.Address, err)
	}
	if adapter == nil {
		return nil, fmt.Errorf("%w: nil adapter from factory for pool %s", ErrInvalidConfig, state.Key.Address)
	}
	amountOut, err := adapter.QuoteExactIn(ctx, state, deep, tokenIn, amountIn)
	if err != nil {
		return nil, fmt.Errorf("simulate: quote pool %s: %w", state.Key.Address, err)
	}
	return amountOut, nil
}

// ExactInputMonotone delegates the optional arithmetic proof to the adapter.
// Unknown adapters retain the full bounded amount search.
func (q *Quoter) ExactInputMonotone(state pool.State) bool {
	adapter, err := q.factory(state)
	if err != nil {
		return false
	}
	proof, ok := adapter.(pool.MonotoneQuoter)
	return ok && proof.ExactInputMonotone(state)
}
