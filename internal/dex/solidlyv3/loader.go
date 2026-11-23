package solidlyv3

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

// Spec identifies a Solidly V3 pool. The fee is not part of it: Load reads the
// current value from slot0.
type Spec struct {
	Address     domain.Address
	Token0      domain.Address
	Token1      domain.Address
	TickSpacing int32
}

var slot0Call = []byte{0x38, 0x50, 0xc7, 0xbd}

// Load reads slot0, liquidity and the tick bitmap words within wordRadius of
// the current tick. The V3 loader decodes the shared fields; the third slot0
// word supplies the fee.
func Load(ctx context.Context, caller pool.ContractCaller, spec Spec, block string, wordRadius int16) (pool.State, pool.DeepState, error) {
	fee, err := ReadFee(ctx, caller, spec.Address, block)
	if err != nil {
		return pool.State{}, pool.DeepState{}, err
	}
	loaded, err := uniswapv3.Load(ctx, caller, v3Spec(spec, fee), block, wordRadius)
	if err != nil {
		return pool.State{}, pool.DeepState{}, fmt.Errorf("solidlyv3: load: %w", err)
	}
	loaded.State.Key.Kind = domain.PoolKindSolidlyV3
	return loaded.State, loaded.Deep, nil
}

// LoadShallow reads slot0 and liquidity without the tick window.
func LoadShallow(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	fee, err := ReadFee(ctx, caller, spec.Address, block)
	if err != nil {
		return pool.State{}, err
	}
	state, err := uniswapv3.LoadShallow(ctx, caller, v3Spec(spec, fee), block)
	if err != nil {
		return pool.State{}, fmt.Errorf("solidlyv3: load: %w", err)
	}
	state.Key.Kind = domain.PoolKindSolidlyV3
	return state, nil
}

func v3Spec(spec Spec, fee uint32) uniswapv3.Spec {
	return uniswapv3.Spec{Address: spec.Address, Token0: spec.Token0, Token1: spec.Token1, Fee: fee, TickSpacing: spec.TickSpacing}
}

// ReadFee reads the fee word of slot0 (sqrtPriceX96, tick, fee, unlocked).
func ReadFee(ctx context.Context, caller pool.ContractCaller, address domain.Address, block string) (uint32, error) {
	var out string
	if err := caller.CallContract(ctx, address, slot0Call, block, &out); err != nil {
		return 0, fmt.Errorf("solidlyv3: slot0: %w", err)
	}
	raw, err := hex.DecodeString(strings.TrimPrefix(out, "0x"))
	if err != nil || len(raw) < 4*32 || len(raw)%32 != 0 {
		return 0, fmt.Errorf("%w: slot0 returned %d bytes", ErrInvalidResponse, len(raw))
	}
	return parseFee(raw[64:96])
}

func parseFee(word []byte) (uint32, error) {
	fee := new(big.Int).SetBytes(word)
	if !fee.IsUint64() || fee.Uint64() > uint64(maxFeePips) {
		return 0, fmt.Errorf("%w: fee %s above the pool cap", ErrInvalidResponse, fee)
	}
	return uint32(fee.Uint64()), nil
}

// TopicSetFee is keccak256("SetFee(uint24,uint24)").
var TopicSetFee = domain.Hash{
	0x0e, 0xb6, 0x3f, 0x4a, 0x36, 0xd6, 0xbd, 0xee,
	0xe0, 0x5a, 0xa0, 0x00, 0x20, 0xa9, 0x7d, 0x80,
	0xc3, 0xe8, 0x4f, 0x1b, 0x5b, 0x3e, 0xbf, 0x34,
	0x5f, 0xb6, 0x72, 0x62, 0xe6, 0x2b, 0x0f, 0x33,
}

// DecodeSetFee returns the new fee of a SetFee log. matched is false for any
// other event, which the V3 reducer then handles.
func DecodeSetFee(topics []domain.Hash, data []byte) (fee uint32, matched bool, err error) {
	if len(topics) == 0 || topics[0] != TopicSetFee {
		return 0, false, nil
	}
	if len(topics) != 1 || len(data) != 64 {
		return 0, true, fmt.Errorf("%w: SetFee has %d topics and %d data bytes", ErrInvalidResponse, len(topics), len(data))
	}
	fee, err = parseFee(data[32:64])
	return fee, true, err
}
