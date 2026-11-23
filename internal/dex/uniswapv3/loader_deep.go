package uniswapv3

import (
	"context"
	"fmt"
	"strconv"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// DeepLoader loads the tick-level state of a V3-shaped pool from the calls the
// pool exposes. It backs pool.DeepLoader for the per-block refresh: the caller
// passes the shallow state it already applied, so no slot0 or liquidity read is
// repeated at the block.
type DeepLoader struct {
	Caller pool.ContractCaller
	// WordRadius is the bitmap word radius used when LoadDeep's wordRadius
	// argument is zero.
	WordRadius int16
}

var _ pool.DeepLoader = (*DeepLoader)(nil)

// LoadDeep reads the bitmap words within wordRadius of state.Tick and every
// initialized tick they contain, in ascending tick order within each word.
//
// It accepts the kinds that use the V3 tick bitmap: V3 and Slipstream, whose
// loaders share the same selectors. Camelot V3 is Algebra and stores ticks in a
// different table, so it is rejected with ErrUnsupportedKind instead of being
// read with the wrong calls.
//
// LoadedBlock records the parsed block number when block is a decimal or
// 0x-prefixed hex number. A block tag such as "latest" leaves it zero because
// the resolved number is unknown to this call.
func (l *DeepLoader) LoadDeep(ctx context.Context, state pool.State, block string, wordRadius int16) (pool.DeepState, error) {
	if err := ctx.Err(); err != nil {
		return pool.DeepState{}, err
	}
	spec, err := deepSpec(state)
	if err != nil {
		return pool.DeepState{}, err
	}
	if wordRadius == 0 {
		wordRadius = l.WordRadius
	}
	if err := validateSpec(l.Caller, spec, block, wordRadius); err != nil {
		return pool.DeepState{}, err
	}

	deep, err := loadDeep(ctx, l.Caller, spec, block, state.Tick, wordRadius)
	if err != nil {
		return pool.DeepState{}, err
	}
	if number, ok := parseBlockNumber(block); ok {
		deep.LoadedBlock = domain.BlockRef{Number: number}
	}
	return deep, nil
}

// deepSpec converts the caller's shallow state into a loader spec, rejecting a
// kind the V3 bitmap calls cannot describe.
func deepSpec(state pool.State) (Spec, error) {
	switch state.Key.Kind {
	case domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindSolidlyV3:
	default:
		// Camelot V3 is Algebra; its tick table is not the V3 bitmap.
		return Spec{}, fmt.Errorf("%w: kind %d for pool %s",
			ErrUnsupportedKind, state.Key.Kind, state.Key.Address)
	}
	return Spec{
		Address:     state.Key.Address,
		Token0:      state.Token0,
		Token1:      state.Token1,
		Fee:         state.Fee,
		TickSpacing: state.TickSpacing,
	}, nil
}

// loadDeep reads the bitmap and ticks in batches when the caller supports them
// and one call at a time otherwise. The returned state is identical.
func loadDeep(ctx context.Context, caller pool.ContractCaller, spec Spec, block string, tick int32, wordRadius int16) (pool.DeepState, error) {
	if batch, ok := caller.(pool.BatchContractCaller); ok {
		return loadDeepBatched(ctx, batch, spec, block, tick, wordRadius)
	}
	return loadDeepSequential(ctx, caller, spec, block, tick, wordRadius)
}

// parseBlockNumber reads the decimal or 0x-prefixed hex block number in block.
// A block tag has no number and reports false.
func parseBlockNumber(block string) (uint64, bool) {
	raw, base := block, 10
	if len(block) > 2 && block[0] == '0' && (block[1] == 'x' || block[1] == 'X') {
		raw, base = block[2:], 16
	}
	number, err := strconv.ParseUint(raw, base, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}
