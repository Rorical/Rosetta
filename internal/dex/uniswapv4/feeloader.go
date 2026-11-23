package uniswapv4

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// FeeLoader refreshes the slot0 fee fields of one V4 pool. It backs the bot's
// per-block fee refresh, which runs after the shallow state is applied and
// before a quote reads it.
type FeeLoader struct {
	Caller pool.ContractCaller
}

// LoadFee reads getSlot0 at block and returns state with Fee and ProtocolFee
// replaced by the live lpFee and packed protocol fee. Price, tick, liquidity,
// and identity stay as supplied: the shallow state already reflects the block,
// and only the fees can change without an event this pipeline decodes.
//
// The call is not batched or shared with the deep refresh. The bot issues one
// read per V4 pool per block, which keeps a fee change from being quoted with
// the previous block's value until restart.
func LoadFee(ctx context.Context, caller pool.ContractCaller, state pool.State, block string) (pool.State, error) {
	if err := ctx.Err(); err != nil {
		return pool.State{}, err
	}
	if state.Key.Address != managerFor(caller) {
		return pool.State{}, fmt.Errorf("%w: V4 pool manager differs from caller deployment", ErrInvalidSpec)
	}
	if state.Key.Kind != domain.PoolKindV4 {
		return pool.State{}, fmt.Errorf("%w: kind %d", ErrUnsupportedKind, state.Key.Kind)
	}
	spec := Spec{
		PoolID:      state.Key.PoolID,
		Token0:      state.Token0,
		Token1:      state.Token1,
		TickSpacing: state.TickSpacing,
		Hooks:       state.Key.Hooks,
	}
	if err := validateSpec(caller, spec, block); err != nil {
		return pool.State{}, err
	}

	slot0Out, err := callStateView(ctx, caller, getSlot0Call, spec.PoolID, block, "getSlot0")
	if err != nil {
		return pool.State{}, err
	}
	slot, err := parseSlot0(slot0Out)
	if err != nil {
		return pool.State{}, err
	}

	updated := state.Copy()
	updated.Fee = slot.lpFee
	updated.ProtocolFee = slot.protocolFee
	return updated, nil
}

// LoadFee implements the bot's fee loader with the caller bound at
// construction.
func (l FeeLoader) LoadFee(ctx context.Context, state pool.State, block string) (pool.State, error) {
	return LoadFee(ctx, l.Caller, state, block)
}
