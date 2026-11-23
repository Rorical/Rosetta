package domain

import (
	"bytes"
	"cmp"
)

// ArbitrumOneChainID is the chain ID of Arbitrum One.
const ArbitrumOneChainID uint64 = 42161

// Token is immutable metadata for an ERC-20 token. Decimals fits in uint8
// because no supported token exceeds 255 decimals.
type Token struct {
	Address  Address
	Symbol   string
	Decimals uint8
}

// PoolKind identifies the protocol family a pool belongs to.
type PoolKind uint8

const (
	PoolKindV2 PoolKind = iota + 1
	PoolKindV3
	PoolKindV4
	// PoolKindCamelotV3 is Camelot's Algebra fork. It shares the V3 swap
	// event and pricing shape but reads globalState() instead of slot0() and
	// has a different tick table layout.
	PoolKindCamelotV3
	// PoolKindSlipstream is the V3-shaped family used by Aerodrome/Velodrome.
	// Its fee module can change the fee without a pool log.
	PoolKindSlipstream
	// PoolKindVelodromeV2 has stable and volatile curves, uint256 reserves,
	// and fees transferred out of the pool before the reserves are updated.
	PoolKindVelodromeV2
	// PoolKindSolidlyV3 is a Uniswap V3 fork. Its fee lives in slot0, changes
	// only through a logged SetFee, and its swap callback has its own name.
	PoolKindSolidlyV3
	// PoolKindMetricOmm is MetricOmmPool, an oracle-anchored market maker
	// with liquidity in a fixed grid of bins and no ticks. Its price comes
	// from an oracle update transaction, not from pool logs. The value 8 is
	// the executor contract's METRIC_OMM leg kind.
	PoolKindMetricOmm
	// PoolKindWrap is the wrapped native token seen as a pool between native
	// currency (the zero address, Token0) and itself (Token1): a deposit one
	// way, a withdrawal the other, always 1:1 and without a fee. Its key's
	// Address is the wrapped token. It has no state on chain and no events.
	// The value 9 is the executor contract's WRAP leg kind.
	PoolKindWrap
)

// UsesTicks identifies the families requiring a loaded liquidity tick window.
func (k PoolKind) UsesTicks() bool {
	return k == PoolKindV3 || k == PoolKindV4 || k == PoolKindCamelotV3 || k == PoolKindSlipstream || k == PoolKindSolidlyV3
}

// PoolKey is the stable identifier of a pool within a chain. Every kind uses
// Address: for V2 and the V3-shaped families it is the pool contract, and for
// V4 it is the PoolManager singleton shared by every V4 pool. V4 pools are told
// apart by PoolID; Hooks records the hook contract that can change swap
// behavior. Both are zero for every other kind.
type PoolKey struct {
	Address Address
	Kind    PoolKind
	PoolID  Hash
	Hooks   Address
}

// IsV4 reports whether the key identifies a Uniswap V4 pool.
func (k PoolKey) IsV4() bool {
	return k.Kind == PoolKindV4
}

// ComparePoolKeys orders keys by address bytes, then kind, then pool ID bytes,
// then hooks bytes. The order is total and independent of map iteration, so it
// is the canonical order for dirty sets, snapshots, and graphs.
func ComparePoolKeys(a, b PoolKey) int {
	if diff := bytes.Compare(a.Address[:], b.Address[:]); diff != 0 {
		return diff
	}
	if diff := cmp.Compare(a.Kind, b.Kind); diff != 0 {
		return diff
	}
	if diff := bytes.Compare(a.PoolID[:], b.PoolID[:]); diff != 0 {
		return diff
	}
	return bytes.Compare(a.Hooks[:], b.Hooks[:])
}

// BlockRef ties a piece of state to an L2 block and the L1 block it depends on.
// Number and Timestamp describe the Arbitrum L2 block; L1Block is the L1 block
// height used by conditional transactions. The two must not be mixed.
type BlockRef struct {
	Number     uint64
	Hash       Hash
	ParentHash Hash
	Timestamp  uint64
	L1Block    uint64
}
