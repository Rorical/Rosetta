package uniswapv4

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// StateView is Uniswap's read-only lens for offchain V4 state reads.
//
// Address source: the Uniswap docs deployment table for Arbitrum One (chain
// 42161), https://github.com/Uniswap/docs/blob/main/content/protocols/v4/deployments.mdx.
// ABI source: https://github.com/Uniswap/v4-periphery/blob/main/src/lens/StateView.sol.
//
// Verified against Arbitrum One: the deployed code at this
// address contains the getSlot0(bytes32) and getLiquidity(bytes32) selectors,
// and both calls return their documented static words.
const (
	StateViewAddress = "0x76fd297e2d437cd7f76d50f01afe6160f86e9990"

	// PoolManagerAddress is the Arbitrum One PoolManager singleton from the
	// same deployment table. Together with the pool ID it identifies one V4
	// pool in pool.State.Key; the address alone identifies the family.
	PoolManagerAddress = "0x360e68faccca8ca495c1b759fd9eee466db9fb32"

	// SelectorGetSlot0 is getSlot0(bytes32).
	SelectorGetSlot0 = "0xc815641c"
	// SelectorGetLiquidity is getLiquidity(bytes32).
	SelectorGetLiquidity = "0xfa6793d5"
)

var (
	stateViewAddress   = domain.Address{0x76, 0xfd, 0x29, 0x7e, 0x2d, 0x43, 0x7c, 0xd7, 0xf7, 0x6d, 0x50, 0xf0, 0x1a, 0xfe, 0x61, 0x60, 0xf8, 0x6e, 0x99, 0x90}
	poolManagerAddress = domain.Address{0x36, 0x0e, 0x68, 0xfa, 0xcc, 0xca, 0x8c, 0xa4, 0x95, 0xc1, 0xb7, 0x59, 0xfd, 0x9e, 0xee, 0x46, 0x6d, 0xb9, 0xfb, 0x32}
	getSlot0Call       = decodeSelector(SelectorGetSlot0)
	getLiquidityCall   = decodeSelector(SelectorGetLiquidity)
)

// PoolManager returns the Arbitrum One PoolManager address as a domain value.
// It is the address PoolManagerAddress names; callers that validate a
// configured V4 pool compare against it instead of parsing the string.
func PoolManager() domain.Address {
	return poolManagerAddress
}

// Spec carries the parts of a V4 pool identity that StateView does not return:
// the pool ID and, when the caller has them from pool creation data, the token
// pair, tick spacing, and hook address. StateView only reports slot0 and active
// liquidity. Token0 may be zero, which is how V4 represents native ETH; token1
// is always a contract address.
//
// V4 pools with non-zero hooks are outside the exact-pricing scope; the loader
// reads their state, but the quote adapter rejects them.
type Spec struct {
	PoolID      domain.Hash
	Token0      domain.Address
	Token1      domain.Address
	TickSpacing int32
	Hooks       domain.Address
}

// Loader reads V4 pool state through StateView. The quote path is Adapter,
// which needs the effective swap fee and rejects hooked pools.
type Loader struct{}

var _ interface {
	Load(context.Context, pool.ContractCaller, Spec, string) (pool.State, error)
	LoadState(context.Context, pool.ContractCaller, domain.Hash, string) (pool.State, error)
} = Loader{}

// LoadState reads slot0 and liquidity for poolID at block, leaving the token
// fields and tick spacing zero because StateView does not report them. The key
// still carries the pool ID, so two pools behind the PoolManager address stay
// distinct.
func LoadState(ctx context.Context, caller pool.ContractCaller, poolID domain.Hash, block string) (pool.State, error) {
	return Load(ctx, caller, Spec{PoolID: poolID}, block)
}

// Load reads slot0 and liquidity at block and fills the identity fields from
// spec. State.Fee is the slot0 lpFee and State.ProtocolFee is the packed slot0
// protocol fee, both in hundredths of a basis point. The swap path combines
// them with SwapFee; keeping them separate preserves the directional protocol
// fee. Dynamic-fee hooks are not represented: their pools report lpFee 0 and
// the hook decides the fee at swap time, which the quote adapter rejects.
//
// The protocol fee in State is a snapshot of slot0 at load time. Swap events
// carry the fee actually charged, but this package does not apply that word, so
// a protocol fee change stays invisible until the pool is loaded again. See
// Adapter for the quoting consequence.
func Load(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	if err := ctx.Err(); err != nil {
		return pool.State{}, err
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

	liquidityOut, err := callStateView(ctx, caller, getLiquidityCall, spec.PoolID, block, "getLiquidity")
	if err != nil {
		return pool.State{}, err
	}
	liquidity, err := parseLiquidity(liquidityOut)
	if err != nil {
		return pool.State{}, err
	}

	return pool.State{
		Key: domain.PoolKey{
			Address: managerFor(caller),
			Kind:    domain.PoolKindV4,
			PoolID:  spec.PoolID,
			Hooks:   spec.Hooks,
		},
		Token0:       spec.Token0,
		Token1:       spec.Token1,
		Fee:          slot.lpFee,
		ProtocolFee:  slot.protocolFee,
		TickSpacing:  spec.TickSpacing,
		SqrtPriceX96: slot.sqrtPriceX96,
		Tick:         slot.tick,
		Liquidity:    liquidity,
	}, nil
}

// Load implements the Loader method form.
func (Loader) Load(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	return Load(ctx, caller, spec, block)
}

// LoadState implements the Loader method form.
func (Loader) LoadState(ctx context.Context, caller pool.ContractCaller, poolID domain.Hash, block string) (pool.State, error) {
	return LoadState(ctx, caller, poolID, block)
}

type slot0 struct {
	sqrtPriceX96 *big.Int
	tick         int32
	protocolFee  uint32
	lpFee        uint32
}

// parseSlot0 reads the four static words of getSlot0: sqrtPriceX96, tick,
// protocolFee, and lpFee. The Uniswap guide describes the two fees as uint8,
// but StateView.sol declares both as uint24; uint24 is the encoding that
// matches the deployed ABI.
func parseSlot0(out string) (slot0, error) {
	values, err := callWords("getSlot0", out, 4)
	if err != nil {
		return slot0{}, err
	}
	sqrtPriceX96, err := unsignedCallWord("getSlot0 sqrtPriceX96", values[0], 160)
	if err != nil {
		return slot0{}, err
	}
	tick, err := signedCallWord("getSlot0 tick", values[1], 24)
	if err != nil {
		return slot0{}, err
	}
	protocolFee, err := unsignedCallWord("getSlot0 protocolFee", values[2], 24)
	if err != nil {
		return slot0{}, err
	}
	lpFee, err := unsignedCallWord("getSlot0 lpFee", values[3], 24)
	if err != nil {
		return slot0{}, err
	}
	return slot0{
		sqrtPriceX96: sqrtPriceX96,
		tick:         tick,
		protocolFee:  uint32(protocolFee.Uint64()),
		lpFee:        uint32(lpFee.Uint64()),
	}, nil
}

func parseLiquidity(out string) (*big.Int, error) {
	values, err := callWords("getLiquidity", out, 1)
	if err != nil {
		return nil, err
	}
	return unsignedCallWord("getLiquidity", values[0], 128)
}

func validateSpec(caller pool.ContractCaller, spec Spec, block string) error {
	if caller == nil {
		return fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if managerFor(caller) == (domain.Address{}) || viewFor(caller) == (domain.Address{}) {
		return fmt.Errorf("%w: V4 deployment is not configured for this chain", ErrInvalidSpec)
	}
	if spec.PoolID == (domain.Hash{}) {
		return fmt.Errorf("%w: zero pool ID", ErrInvalidSpec)
	}
	if block == "" {
		return fmt.Errorf("%w: empty block", ErrInvalidSpec)
	}
	if spec.TickSpacing < 0 {
		return fmt.Errorf("%w: tick spacing %d must not be negative", ErrInvalidSpec, spec.TickSpacing)
	}
	zeroToken0 := spec.Token0 == (domain.Address{})
	zeroToken1 := spec.Token1 == (domain.Address{})
	// Both zero means the caller does not know the pair (LoadState). A zero
	// token0 with a token1 set is native ETH, which V4 permits as currency0;
	// token1 is never native.
	if !zeroToken0 && zeroToken1 {
		return fmt.Errorf("%w: token1 is zero while token0 is set", ErrInvalidSpec)
	}
	if !zeroToken0 && spec.Token0 == spec.Token1 {
		return fmt.Errorf("%w: token0 and token1 are equal", ErrInvalidSpec)
	}
	return nil
}

// callStateView appends the pool ID to the selector. It allocates a new slice
// so the package-level selector is never mutated.
func callStateView(ctx context.Context, caller pool.ContractCaller, selector []byte, poolID domain.Hash, block, name string) (string, error) {
	return callStateViewWord(ctx, caller, selector, poolID, nil, block, name)
}

// callWords decodes a hex result containing at least minWords 32-byte words.
// Extra words are ignored because newer StateView versions may append fields.
func callWords(name, out string, minWords int) ([][]byte, error) {
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
	words := make([][]byte, len(raw)/abiWordSize)
	for i := range words {
		words[i] = raw[i*abiWordSize : (i+1)*abiWordSize]
	}
	return words, nil
}

func unsignedCallWord(name string, raw []byte, bits int) (*big.Int, error) {
	value := new(big.Int).SetBytes(raw)
	if value.BitLen() > bits {
		return nil, fmt.Errorf("%w: %s %s does not fit uint%d", ErrInvalidResponse, name, value, bits)
	}
	return value, nil
}

// signedCallWord reads a two's-complement ABI word and rejects values outside
// int<bits>.
func signedCallWord(name string, raw []byte, bits int) (int32, error) {
	value := new(big.Int).SetBytes(raw)
	if raw[0]&0x80 != 0 {
		value.Sub(value, twoTo256)
	}
	minimum := new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)))
	maximum := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits-1)), big.NewInt(1))
	if value.Cmp(minimum) < 0 || value.Cmp(maximum) > 0 {
		return 0, fmt.Errorf("%w: %s %s does not fit int%d", ErrInvalidResponse, name, value, bits)
	}
	return int32(value.Int64()), nil
}
