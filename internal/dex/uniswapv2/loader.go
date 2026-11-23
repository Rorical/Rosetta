package uniswapv2

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// SelectorGetReserves is the selector of the V2 pair getReserves() call.
const SelectorGetReserves = "0x0902f1ac"

const abiWordSize = 32

// Spec identifies a constant-product pool. Fee is the pool-level swap fee in
// pips; it is not readable from getReserves, so the caller supplies it.
type Spec struct {
	Address domain.Address
	Token0  domain.Address
	Token1  domain.Address
	Fee     uint32
}

// getReservesCall is the decoded selector literal; tests assert it is non-nil.
var getReservesCall = decodeSelector(SelectorGetReserves)

// Load reads getReserves at block and returns the pool's shallow state.
func Load(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	if err := ctx.Err(); err != nil {
		return pool.State{}, err
	}
	if err := validateSpec(caller, spec, block); err != nil {
		return pool.State{}, err
	}

	var out string
	if err := caller.CallContract(ctx, spec.Address, getReservesCall, block, &out); err != nil {
		return pool.State{}, fmt.Errorf("uniswapv2: getReserves: %w", err)
	}
	reserve0, reserve1, err := ParseReserves(out)
	if err != nil {
		return pool.State{}, err
	}
	return pool.State{Key: domain.PoolKey{Address: spec.Address, Kind: domain.PoolKindV2}, Token0: spec.Token0, Token1: spec.Token1,
		Fee: spec.Fee, Reserve0: reserve0, Reserve1: reserve1}, nil
}

// ParseReserves decodes the uint112 reserve words shared by loading and
// discovery sizing. The trailing timestamp does not affect quote state.
func ParseReserves(out string) (*big.Int, *big.Int, error) {
	values, err := decodeWords("getReserves", out, 2)
	if err != nil {
		return nil, nil, err
	}
	reserve0, err := unsignedWord("getReserves reserve0", values[0], 112)
	if err != nil {
		return nil, nil, err
	}
	reserve1, err := unsignedWord("getReserves reserve1", values[1], 112)
	if err != nil {
		return nil, nil, err
	}
	return reserve0, reserve1, nil
}

func validateSpec(caller pool.ContractCaller, spec Spec, block string) error {
	if caller == nil {
		return fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if spec.Address == (domain.Address{}) {
		return fmt.Errorf("%w: zero pool address", ErrInvalidSpec)
	}
	if spec.Token0 == (domain.Address{}) {
		return fmt.Errorf("%w: zero token0", ErrInvalidSpec)
	}
	if spec.Token1 == (domain.Address{}) {
		return fmt.Errorf("%w: zero token1", ErrInvalidSpec)
	}
	if spec.Token0 == spec.Token1 {
		return fmt.Errorf("%w: token0 and token1 are equal", ErrInvalidSpec)
	}
	if spec.Fee >= maxFeePips {
		return fmt.Errorf("%w: fee %d pips outside [0, %d)", ErrInvalidSpec, spec.Fee, maxFeePips)
	}
	if block == "" {
		return fmt.Errorf("%w: empty block", ErrInvalidSpec)
	}
	return nil
}

// decodeWords decodes a hex result containing at least minWords 32-byte words.
// getReserves has three static words; extra words are ignored.
func decodeWords(name, out string, minWords int) ([][]byte, error) {
	rawHex := strings.TrimPrefix(out, "0x")
	if rawHex == "" {
		return nil, fmt.Errorf("%w: %s returned empty data", ErrInvalidResponse, name)
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not hex: %v", ErrInvalidResponse, name, err)
	}
	if len(raw) < minWords*abiWordSize || len(raw)%abiWordSize != 0 {
		return nil, fmt.Errorf("%w: %s returned %d bytes, want at least %d whole words",
			ErrInvalidResponse, name, len(raw), minWords)
	}
	values := make([][]byte, len(raw)/abiWordSize)
	for i := range values {
		values[i] = raw[i*abiWordSize : (i+1)*abiWordSize]
	}
	return values, nil
}

func unsignedWord(name string, raw []byte, bits int) (*big.Int, error) {
	value := new(big.Int).SetBytes(raw)
	if value.BitLen() > bits {
		return nil, fmt.Errorf("%w: %s %s does not fit uint%d", ErrInvalidResponse, name, value, bits)
	}
	return value, nil
}

func decodeSelector(selector string) []byte {
	raw, err := hex.DecodeString(strings.TrimPrefix(selector, "0x"))
	if err != nil {
		return nil
	}
	return raw
}
