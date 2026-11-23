package slipstream

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// SelectorFee is fee() on a Slipstream pool. The value is dynamic on Arbitrum:
// the factory can change it between blocks, and the static tick-spacing tier is
// only a fallback the pool reports when no module overrides it.
const SelectorFee = "0xddca3f43"

// maxFeePips is 100% in hundredths of a basis point, the unit fee() returns and
// the same unit internal/math expects. A value at or above it cannot be a fee.
const maxFeePips uint32 = 1_000_000

var feeCall = decodeSelector(SelectorFee)

// Spec identifies a Slipstream pool. Fee is the creation-time fee used to
// validate the request; Load returns the live fee() value instead. TickSpacing
// is the pool-level spacing. Neither fee() nor the slot0/liquidity calls return
// the tick spacing, so the caller supplies it from pool creation data.
type Spec struct {
	Address     domain.Address
	Token0      domain.Address
	Token1      domain.Address
	Fee         uint32
	TickSpacing int32
}

// Load reads fee(), slot0, liquidity, and the tick bitmap words within
// wordRadius of the current tick. Slipstream uses the V3 state layout and the
// same 1e-6 fee unit, so the state work is delegated to uniswapv3.Load; the
// live fee() read replaces the spec fee and the state is tagged with
// PoolKindSlipstream so routing can tell the two families apart.
//
// A fee() call that fails or returns malformed data fails the load. Falling
// back to the spec fee would quote with a stale tier while the pool charges the
// dynamic value.
func Load(ctx context.Context, caller pool.ContractCaller, spec Spec, block string, wordRadius int16) (pool.State, pool.DeepState, error) {
	loaded, err := uniswapv3.Load(ctx, caller, uniswapv3.Spec{
		Address:     spec.Address,
		Token0:      spec.Token0,
		Token1:      spec.Token1,
		Fee:         spec.Fee,
		TickSpacing: spec.TickSpacing,
	}, block, wordRadius)
	if err != nil {
		return pool.State{}, pool.DeepState{}, fmt.Errorf("slipstream: load: %w", err)
	}

	fee, err := loadFee(ctx, caller, spec.Address, block)
	if err != nil {
		return pool.State{}, pool.DeepState{}, fmt.Errorf("slipstream: load: %w", err)
	}

	loaded.State.Key.Kind = domain.PoolKindSlipstream
	loaded.State.Fee = fee
	if local, ok := caller.(LocalStateCaller); ok {
		if err := loadLocal(ctx, local, &loaded.State, block); err != nil {
			return pool.State{}, pool.DeepState{}, err
		}
	}
	return loaded.State, loaded.Deep, nil
}

// loadFee reads fee() as one uint24 word.
func loadFee(ctx context.Context, caller pool.ContractCaller, address domain.Address, block string) (uint32, error) {
	var out string
	if err := caller.CallContract(ctx, address, feeCall, block, &out); err != nil {
		return 0, fmt.Errorf("fee: %w", err)
	}
	return parseFee(out)
}

func parseFee(out string) (uint32, error) {
	raw, err := hex.DecodeString(strings.TrimPrefix(out, "0x"))
	if err != nil {
		return 0, fmt.Errorf("%w: fee is not hex: %v", ErrInvalidResponse, err)
	}
	if len(raw) != 32 {
		return 0, fmt.Errorf("%w: fee returned %d bytes, want 32", ErrInvalidResponse, len(raw))
	}
	value := new(big.Int).SetBytes(raw)
	if !value.IsUint64() || value.Uint64() >= uint64(maxFeePips) {
		return 0, fmt.Errorf("%w: fee %s outside [0, %d)", ErrInvalidResponse, value, maxFeePips)
	}
	return uint32(value.Uint64()), nil
}

// decodeSelector decodes a four-byte selector. The literal constants are
// checked by the selector test, so a malformed one fails there rather than
// changing a call's meaning at runtime.
func decodeSelector(selector string) []byte {
	raw, err := hex.DecodeString(strings.TrimPrefix(selector, "0x"))
	if err != nil || len(raw) != 4 {
		return nil
	}
	return raw
}
