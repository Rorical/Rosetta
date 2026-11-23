package slipstream

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// FeeLoader refreshes the dynamic fee() of one Slipstream pool. It backs the
// bot's per-block fee refresh and keeps the static tick-spacing tier from being
// quoted after the factory changes the fee.
type FeeLoader struct {
	Caller pool.ContractCaller
}

// LoadFee reads fee() at block and returns state with Fee replaced by the live
// value. Explicit refreshes reload the model: block number alone cannot identify
// its branch. A fee-only caller cannot retain an older oracle beside a newer
// fee. Normal log following uses LocalFee without invoking this loader.
// Price/liquidity supplied by the caller must reflect the block.
func LoadFee(ctx context.Context, caller pool.ContractCaller, state pool.State, block string) (pool.State, error) {
	if err := ctx.Err(); err != nil {
		return pool.State{}, err
	}
	if caller == nil {
		return pool.State{}, fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if state.Key.Kind != domain.PoolKindSlipstream {
		return pool.State{}, fmt.Errorf("%w: kind %d is not Slipstream", ErrInvalidSpec, state.Key.Kind)
	}
	if state.Key.Address == (domain.Address{}) {
		return pool.State{}, fmt.Errorf("%w: zero pool address", ErrInvalidSpec)
	}
	if block == "" {
		return pool.State{}, fmt.Errorf("%w: empty block", ErrInvalidSpec)
	}

	fee, err := loadFee(ctx, caller, state.Key.Address, block)
	if err != nil {
		return pool.State{}, err
	}
	updated := state.Copy()
	updated.Fee = fee
	updated.Slipstream = nil
	if local, ok := caller.(LocalStateCaller); ok {
		if err := loadLocal(ctx, local, &updated, block); err != nil {
			return pool.State{}, err
		}
	}
	return updated, nil
}

// LoadFee implements the bot's fee loader with the caller bound at
// construction.
func (l FeeLoader) LoadFee(ctx context.Context, state pool.State, block string) (pool.State, error) {
	return LoadFee(ctx, l.Caller, state, block)
}
