package metricomm

import (
	"context"
	"errors"
	"fmt"
	"math/big"

	"github.com/holiman/uint256"

	"github.com/Rorical/Rosetta/internal/domain"
	poolmath "github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

var (
	// ErrStaleState means the pool emitted an event the log replay does not
	// model; its storage words must be read again before it can be quoted.
	ErrStaleState = errors.New("metricomm: storage words are stale")
	// ErrNoQuote means no valid oracle quote is known for the pool.
	ErrNoQuote = errors.New("metricomm: no oracle quote")
	// ErrPartialFill means the pool cannot take the whole input. The executor
	// contract reverts on a partial fill, so there is no amount to quote.
	ErrPartialFill = errors.New("metricomm: input not fully consumed")
)

var noBuyPriceLimit = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))

// Adapter quotes a MetricOmmPool from the raw state in pool.State.Metric.
type Adapter struct{}

var _ pool.Adapter = (*Adapter)(nil)

// New returns the adapter. It keeps nothing: bins, fees and the quote all
// change between quotes and are read from each state.
func New() *Adapter {
	return &Adapter{}
}

// Kind reports the pool family this adapter prices.
func (a *Adapter) Kind() domain.PoolKind {
	return domain.PoolKindMetricOmm
}

// QuoteExactIn returns the output of selling amountIn of tokenIn, as the pool
// computes it for an exact-input swap without a price limit. It does not
// model the pool's extensions, which can still reject the swap on chain.
//
// It does not certify pool.MonotoneQuoter: the in-bin step solves a square
// root and rescales, and that rounding has not been shown monotone.
func (a *Adapter) QuoteExactIn(ctx context.Context, state pool.State, _ pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if state.Key.Kind != domain.PoolKindMetricOmm {
		return nil, fmt.Errorf("%w: pool kind %d is not MetricOmm", pool.ErrInvalidState, state.Key.Kind)
	}
	var zeroForOne bool
	switch tokenIn {
	case state.Token0:
		zeroForOne = true
	case state.Token1:
		zeroForOne = false
	default:
		return nil, fmt.Errorf("%w: input token %s outside pool", pool.ErrInvalidState, tokenIn)
	}
	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount in must be positive", ErrInvalidInput)
	}
	metric := state.Metric
	if metric == nil {
		return nil, fmt.Errorf("%w: no MetricOmm state", pool.ErrInvalidState)
	}
	if metric.Stale {
		return nil, ErrStaleState
	}
	if metric.BidX64 == nil || metric.AskX64 == nil {
		return nil, ErrNoQuote
	}
	decoded, err := decodeMetricState(metric)
	if err != nil {
		return nil, err
	}

	// The reference only has to lie inside the quote: it anchors the bin
	// prices and the base fee in opposite directions and cancels out of the
	// amounts up to rounding in the last bits. The geometric mean also
	// satisfies any asymmetry bound the pool might enable.
	reference := new(big.Int).Mul(metric.BidX64, metric.AskX64)
	reference.Sqrt(reference)
	priceLimit := noBuyPriceLimit
	if zeroForOne {
		priceLimit = new(big.Int)
	}
	amount0, amount1, _, err := Swap(decoded, zeroForOne, amountIn, priceLimit, metric.BidX64, metric.AskX64, reference)
	if err != nil {
		return nil, err
	}
	consumed, received := amount0, amount1
	if !zeroForOne {
		consumed, received = amount1, amount0
	}
	if consumed.Cmp(amountIn) != 0 {
		// A sizing bound, as for a V3 pool: the search shrinks the input and
		// does not treat the pool as needing a refresh.
		return nil, fmt.Errorf("%w: %w: %s of %s", poolmath.ErrCapacity, ErrPartialFill, consumed, amountIn)
	}
	return received.Neg(received), nil
}

func decodeMetricState(metric *pool.MetricState) (State, error) {
	immutables, err := immutablesOf(metric)
	if err != nil {
		return State{}, err
	}
	return DecodeState(immutables, metric.Words)
}

func immutablesOf(metric *pool.MetricState) (Immutables, error) {
	scale0, err := scaleMultiplier("token0", metric.Token0Scale)
	if err != nil {
		return Immutables{}, err
	}
	scale1, err := scaleMultiplier("token1", metric.Token1Scale)
	if err != nil {
		return Immutables{}, err
	}
	return Immutables{
		Token0ScaleMultiplier: *scale0,
		Token1ScaleMultiplier: *scale1,
		LowestBin:             metric.LowestBin,
		HighestBin:            metric.HighestBin,
	}, nil
}

func scaleMultiplier(name string, value *big.Int) (*uint256.Int, error) {
	if value == nil || value.Sign() <= 0 {
		return nil, fmt.Errorf("%w: %s scale multiplier must be positive", pool.ErrInvalidState, name)
	}
	converted, overflow := uint256.FromBig(value)
	if overflow {
		return nil, fmt.Errorf("%w: %s scale multiplier outside uint256", pool.ErrInvalidState, name)
	}
	return converted, nil
}
