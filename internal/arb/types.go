package arb

import (
	"context"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

// View is a read-only snapshot of pool state indexed by pool key.
type View interface {
	// Pool returns the shallow state of the pool at key.
	Pool(key domain.PoolKey) (pool.State, bool)
	// Deep returns the tick-level state of the pool at key. V3-style pools
	// require it; V2-style pools ignore it.
	Deep(key domain.PoolKey) (pool.DeepState, bool)
}

// CycleTable is the read-only cycle store the engine evaluates. The shallow
// Table and the deeper DeepTable both implement it.
type CycleTable interface {
	Len() int
	Cycle(i int) graph.Cycle
	Candidates(dirty []domain.PoolKey, k int) []int
}

// Quoter prices one exact-input swap against a pool state. The engine injects
// a Quoter so it does not depend on any concrete protocol adapter.
// Implementations must not mutate state or deep.
type Quoter interface {
	QuoteExactIn(ctx context.Context, state pool.State, deep pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error)
}

// Leg is one quoted swap in an evaluated cycle. AmountIn and AmountOut are the
// gross token amounts entering and leaving Pool.
type Leg struct {
	Pool      domain.PoolKey
	AmountIn  *big.Int
	AmountOut *big.Int
}

// Opportunity is a cycle quoted at the engine's chosen input. Net is Output
// minus Input before gas and may be negative. GasCost is the engine's fixed
// gas estimate in the gas token's base units, zero when no gas is configured.
//
// NetAfterGas is Net minus GasCost only when the cycle starts in the gas token.
// Otherwise the two numbers are in different denominations, GasComparable is
// false, and NetAfterGas equals Net.
// A loss quote can end at the first leg whose output rounded to zero.
type Opportunity struct {
	Cycle         graph.Cycle
	Input         *big.Int
	Output        *big.Int
	Net           *big.Int
	GasCost       *big.Int
	NetAfterGas   *big.Int
	GasComparable bool
	Legs          []Leg
}

// GasConfig is a fixed gas estimate charged against every opportunity. Units is
// the gas amount and Price the cost per gas unit in Token's base units; a nil
// or non-positive Price means zero cost. Token is the token the price is
// denominated in, so a cost can only be subtracted from a cycle that starts in
// the same token; the zero address means native ETH.
type GasConfig struct {
	Units uint64
	Price *big.Int
	Token domain.Address
}

// NotionalFunc returns the input amount used to quote a cycle starting at
// startToken. It must return a positive amount for every start token that
// should be evaluated; non-positive results skip the cycle.
type NotionalFunc func(startToken domain.Address) *big.Int
