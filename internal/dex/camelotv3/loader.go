package camelotv3

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Function selectors of the Camelot V3 (Algebra) read-only calls used by Load.
const (
	SelectorGlobalState = "0xe76c01e4"
	SelectorLiquidity   = "0x1a686502"
)

const abiWordSize = 32

// Spec identifies a Camelot V3 pool. Fee is the pool-level fee in hundredths
// of a basis point (1e-6), the unit internal/math expects. TickSpacing is not
// returned by the calls Load makes, so the caller supplies observed spacing.
// Fee is a legacy six-word fallback; eight-word pools preserve both observed
// fees, including zero, without substituting configured values.
type Spec struct {
	Address     domain.Address
	Token0      domain.Address
	Token1      domain.Address
	Fee         uint32
	TickSpacing int32
}

// Selector constants are package literals; tests assert that each one decodes.
var (
	globalStateCall = decodeSelector(SelectorGlobalState)
	liquidityCall   = decodeSelector(SelectorLiquidity)
	twoTo256        = new(big.Int).Lsh(big.NewInt(1), 256)
)

// Load reads globalState and liquidity at block and returns the pool's shallow
// state with the Camelot V3 pool kind. globalState's fee is in hundredths of a
// basis point, the same unit internal/math uses. The on-chain fee wins when it
// is non-zero; spec.Fee is the fallback for a pool that has not published a fee
// yet in the legacy six-word layout. Eight-word directional fees are exact
// observations and always win, including zero.
func Load(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	if err := ctx.Err(); err != nil {
		return pool.State{}, err
	}
	if err := validateSpec(caller, spec, block); err != nil {
		return pool.State{}, err
	}

	var globalOut string
	if err := caller.CallContract(ctx, spec.Address, globalStateCall, block, &globalOut); err != nil {
		return pool.State{}, fmt.Errorf("camelotv3: globalState: %w", err)
	}
	global, err := parseGlobalState(globalOut)
	if err != nil {
		return pool.State{}, err
	}

	var liquidityOut string
	if err := caller.CallContract(ctx, spec.Address, liquidityCall, block, &liquidityOut); err != nil {
		return pool.State{}, fmt.Errorf("camelotv3: liquidity: %w", err)
	}
	liquidity, err := parseLiquidity(liquidityOut)
	if err != nil {
		return pool.State{}, err
	}

	fee := spec.Fee
	if global.fee != 0 || global.reverse != nil {
		fee = global.fee
	}
	return pool.State{
		Key:           domain.PoolKey{Address: spec.Address, Kind: domain.PoolKindCamelotV3},
		Token0:        spec.Token0,
		Token1:        spec.Token1,
		Fee:           fee,
		FeeOneForZero: global.reverse,
		TickSpacing:   spec.TickSpacing,
		SqrtPriceX96:  global.price,
		Tick:          global.tick,
		Liquidity:     liquidity,
	}, nil
}

type globalState struct {
	price   *big.Int
	tick    int32
	fee     uint32
	reverse *uint32
}

// Camelot V3 has eight words with two directional fees. The existing six-word
// Integral-shaped ABI has one fee. Other layouts need explicit support rather
// than guessing whether word three is a fee, a timepoint or a plugin bitmap.
func parseGlobalState(out string) (globalState, error) {
	values, err := decodeWords("globalState", out, 6)
	if err != nil {
		return globalState{}, err
	}
	if len(values) != 6 && len(values) != 8 {
		return globalState{}, fmt.Errorf("%w: globalState has unsupported %d-word layout", ErrInvalidResponse, len(values))
	}
	sqrtPriceX96, err := unsignedWord("globalState price", values[0], 160)
	if err != nil {
		return globalState{}, err
	}
	rawTick, err := signedWord("globalState tick", values[1], 24)
	if err != nil {
		return globalState{}, err
	}
	fee, err := unsignedWord("globalState fee", values[2], 16)
	if err != nil {
		return globalState{}, err
	}
	global := globalState{price: sqrtPriceX96, tick: int32(rawTick.Int64()), fee: uint32(fee.Uint64())}
	if len(values) == 8 {
		reverse, err := unsignedWord("globalState reverse fee", values[3], 16)
		if err != nil {
			return globalState{}, err
		}
		fee := uint32(reverse.Uint64())
		global.reverse = &fee
	}
	return global, nil
}

func parseLiquidity(out string) (*big.Int, error) {
	values, err := decodeWords("liquidity", out, 1)
	if err != nil {
		return nil, err
	}
	return unsignedWord("liquidity", values[0], 128)
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
	if spec.TickSpacing <= 0 {
		return fmt.Errorf("%w: tick spacing %d must be positive", ErrInvalidSpec, spec.TickSpacing)
	}
	if block == "" {
		return fmt.Errorf("%w: empty block", ErrInvalidSpec)
	}
	return nil
}

// decodeWords decodes a hex result containing at least minWords 32-byte words.
// Extra words are ignored because Algebra versions append fields to
// globalState.
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

// unsignedWord reads an unsigned ABI word and rejects values wider than bits.
func unsignedWord(name string, raw []byte, bits int) (*big.Int, error) {
	value := new(big.Int).SetBytes(raw)
	if value.BitLen() > bits {
		return nil, fmt.Errorf("%w: %s %s does not fit uint%d", ErrInvalidResponse, name, value, bits)
	}
	return value, nil
}

// signedWord reads a two's-complement ABI word and rejects values outside
// int<bits>.
func signedWord(name string, raw []byte, bits int) (*big.Int, error) {
	value := new(big.Int).SetBytes(raw)
	if raw[0]&0x80 != 0 {
		value.Sub(value, twoTo256)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	minimum := new(big.Int).Neg(new(big.Int).Set(limit))
	maximum := new(big.Int).Sub(limit, big.NewInt(1))
	if value.Cmp(minimum) < 0 || value.Cmp(maximum) > 0 {
		return nil, fmt.Errorf("%w: %s %s does not fit int%d", ErrInvalidResponse, name, value, bits)
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
