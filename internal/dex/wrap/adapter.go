// Package wrap prices the wrapped native token as a pool between native
// currency and itself, so a route can cross from one to the other.
package wrap

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// rankReserveBits sizes the reserves of the screening model: a product curve
// this deep loses less than one unit on any 256-bit amount's first 100 bits,
// so coarse ranking sees the 1:1 rate at every size.
const rankReserveBits = 200

// State is the pool of the wrapped native token: native currency (the zero
// address) is Token0, the token itself Token1 and the key's address. It has
// no fee and nothing on chain to load or follow.
func State(wrapped domain.Address) pool.State {
	return pool.State{Key: Key(wrapped), Token1: wrapped}
}

// Key identifies the pool of the wrapped native token.
func Key(wrapped domain.Address) domain.PoolKey {
	return domain.PoolKey{Address: wrapped, Kind: domain.PoolKindWrap}
}

// Adapter quotes a deposit or a withdrawal: the output is the input.
type Adapter struct{}

var (
	_ pool.Adapter        = (*Adapter)(nil)
	_ pool.MonotoneQuoter = (*Adapter)(nil)
)

// New returns the adapter. It keeps nothing.
func New() *Adapter {
	return &Adapter{}
}

// Kind reports the pool family this adapter prices.
func (a *Adapter) Kind() domain.PoolKind {
	return domain.PoolKindWrap
}

// QuoteExactIn returns amountIn for either direction of a well-formed pool.
func (a *Adapter) QuoteExactIn(ctx context.Context, state pool.State, _ pool.DeepState, tokenIn domain.Address, amountIn *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := Validate(state); err != nil {
		return nil, err
	}
	if tokenIn != state.Token0 && tokenIn != state.Token1 {
		return nil, fmt.Errorf("%w: input token %s outside pool", pool.ErrInvalidState, tokenIn)
	}
	if amountIn == nil || amountIn.Sign() <= 0 {
		return nil, fmt.Errorf("%w: amount in must be positive", pool.ErrInvalidState)
	}
	return new(big.Int).Set(amountIn), nil
}

// ExactInputMonotone certifies the identity quote.
func (a *Adapter) ExactInputMonotone(pool.State) bool {
	return true
}

// Validate checks that state is the pool State builds: the executor's WRAP
// leg calls the key's address, so any other token pair would quote a
// conversion the contract refuses.
func Validate(state pool.State) error {
	key := state.Key
	if key.Kind != domain.PoolKindWrap || key.Address == (domain.Address{}) || key.PoolID != (domain.Hash{}) || key.Hooks != (domain.Address{}) {
		return fmt.Errorf("%w: pool kind %d is not a wrap of its address", pool.ErrInvalidState, key.Kind)
	}
	if state.Token0 != (domain.Address{}) || state.Token1 != key.Address || state.Fee != 0 {
		return fmt.Errorf("%w: a wrap pairs native currency with %s at no fee", pool.ErrInvalidState, key.Address)
	}
	return nil
}

// RankingModel is the screening model of the pool: equal reserves too deep
// for any amount to move. It is not a constant-product pool, so it takes no
// part in constant-product proofs; exact amounts come from the adapter.
func RankingModel(state pool.State) (pool.RankingModel, error) {
	if err := Validate(state); err != nil {
		return pool.RankingModel{}, err
	}
	reserve := new(big.Int).Lsh(big.NewInt(1), rankReserveBits)
	return pool.RankingModel{Reserve0: reserve, Reserve1: new(big.Int).Set(reserve)}, nil
}
