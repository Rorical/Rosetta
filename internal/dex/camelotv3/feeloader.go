package camelotv3

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// LoadFee refreshes globalState's current fee without replacing receipt-derived
// price/liquidity. Zero is kept as observed rather than replaced by an old fee.
func LoadFee(ctx context.Context, caller pool.ContractCaller, state pool.State, block string) (pool.State, error) {
	if err := ctx.Err(); err != nil {
		return pool.State{}, err
	}
	if state.Key.Kind != domain.PoolKindCamelotV3 {
		return pool.State{}, fmt.Errorf("%w: fee loader kind %d", ErrInvalidSpec, state.Key.Kind)
	}
	spec := Spec{Address: state.Key.Address, Token0: state.Token0, Token1: state.Token1, Fee: state.Fee, TickSpacing: state.TickSpacing}
	if err := validateSpec(caller, spec, block); err != nil {
		return pool.State{}, err
	}
	var output string
	if err := caller.CallContract(ctx, state.Key.Address, globalStateCall, block, &output); err != nil {
		return pool.State{}, fmt.Errorf("camelotv3: fee globalState: %w", err)
	}
	global, err := parseGlobalState(output)
	if err != nil {
		return pool.State{}, err
	}
	updated := state.Copy()
	updated.Fee, updated.FeeOneForZero = global.fee, global.reverse
	return updated, nil
}
