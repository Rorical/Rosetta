package pool

import (
	"context"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Adapter quotes a pool without mutating shared state. Implementations may
// read state and deep but must not modify them; callers pass copies when they
// need isolation.
type Adapter interface {
	Kind() domain.PoolKind
	QuoteExactIn(ctx context.Context, state State, deep DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error)
}

// RankingModel is a protocol's shallow screening approximation in raw token
// units. ConstantProduct permits CP-specific rejection proofs; other models
// only suggest candidate order/amounts and require an exact adapter quote.
type RankingModel struct {
	Reserve0, Reserve1 *big.Int
	// Optional token1-to-token0 curve; nil uses Reserve1/Reserve0.
	// Stable invariants need a distinct input reserve in each direction to
	// preserve the spot derivative and cap output at actual inventory.
	ReverseIn, ReverseOut *big.Int
	ConstantProduct       bool
}

// MonotoneQuoter optionally certifies fixed-state exact-input quotes: output
// cannot decrease as input increases. Tick-window or missing-tick failures
// beyond a valid starting tick also remain unavailable at larger inputs.
// Amount-dependent hooks or unverified rounding models must not opt in.
type MonotoneQuoter interface {
	ExactInputMonotone(State) bool
}
