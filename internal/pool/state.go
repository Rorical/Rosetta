package pool

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
)

// TickInfo aliases math.TickInfo so adapters can use one tick representation.
type TickInfo = math.TickInfo

// State is the shallow, slot0-level state of a pool. Reserve0 and Reserve1 are
// used by constant-product pools; SqrtPriceX96, Tick, and Liquidity by
// concentrated-liquidity pools. Fee is the symmetric or zero-for-one LP fee;
// FeeOneForZero overrides the reverse direction for Camelot. ProtocolFee
// is V4's packed slot0 protocol fee, with the zero-for-one half in the low 12
// bits and the one-for-zero half in the high 12, and is zero for other kinds.
type State struct {
	Key    domain.PoolKey
	Token0 domain.Address
	Token1 domain.Address
	Fee    uint32
	// Camelot's reverse LP fee. Nil means the symmetric Fee applies; a
	// non-nil zero is an observed zero fee, not a missing value.
	FeeOneForZero *uint32
	ProtocolFee   uint32
	TickSpacing   int32
	SqrtPriceX96  *big.Int
	Tick          int32
	Liquidity     *big.Int
	Reserve0      *big.Int
	Reserve1      *big.Int
	UpdatedBlock  uint64
	Velodrome     *VelodromeState
	Slipstream    *SlipstreamState
	Metric        *MetricState
}

// VelodromeState holds immutable metadata read from Pool.metadata(). Scales are
// powers of ten, not decimal counts. Factory supplies the live per-pool fee.
type VelodromeState struct {
	Factory domain.Address
	Stable  bool
	Scale0  *big.Int
	Scale1  *big.Int
}

// DeepState is the tick-level state a V3-style quote needs in addition to
// State. Bitmap maps word positions to 256-bit words; Ticks maps initialized
// ticks to their liquidity. WindowMin and WindowMax are the inclusive tick
// bounds the loaded bitmap covers, so a quote can tell whether a shallow tick
// move stayed inside the loaded window. Zero/zero means the window is unknown,
// which an old snapshot or an event-replayed deep state can produce.
// LoadedBlock records the block the ticks were loaded at, so a quote can tell
// stale snapshot state from replay-derived state; it is zero when the ticks
// were built by event replay.
type DeepState struct {
	Bitmap      map[int16]*big.Int
	Ticks       map[int32]TickInfo
	WindowMin   int32
	WindowMax   int32
	LoadedBlock domain.BlockRef
}

// Copy returns a deep copy of s. Big integers are cloned; nil stays nil.
func (s State) Copy() State {
	copied := s
	if s.Slipstream != nil {
		metadata := *s.Slipstream
		copied.Slipstream = &metadata
	}
	if s.FeeOneForZero != nil {
		fee := *s.FeeOneForZero
		copied.FeeOneForZero = &fee
	}
	copied.SqrtPriceX96 = cloneInt(s.SqrtPriceX96)
	copied.Liquidity = cloneInt(s.Liquidity)
	copied.Reserve0 = cloneInt(s.Reserve0)
	copied.Reserve1 = cloneInt(s.Reserve1)
	if s.Velodrome != nil {
		metadata := *s.Velodrome
		metadata.Scale0 = cloneInt(metadata.Scale0)
		metadata.Scale1 = cloneInt(metadata.Scale1)
		copied.Velodrome = &metadata
	}
	copied.Metric = s.Metric.Copy()
	return copied
}

// Copy returns a deep copy of d. Maps and every contained big.Int are cloned.
func (d DeepState) Copy() DeepState {
	copied := DeepState{
		LoadedBlock: d.LoadedBlock,
		WindowMin:   d.WindowMin,
		WindowMax:   d.WindowMax,
	}
	if d.Bitmap != nil {
		copied.Bitmap = make(map[int16]*big.Int, len(d.Bitmap))
		for position, word := range d.Bitmap {
			copied.Bitmap[position] = cloneInt(word)
		}
	}
	if d.Ticks != nil {
		copied.Ticks = make(map[int32]TickInfo, len(d.Ticks))
		for tick, info := range d.Ticks {
			copied.Ticks[tick] = TickInfo{
				LiquidityGross: cloneInt(info.LiquidityGross),
				LiquidityNet:   cloneInt(info.LiquidityNet),
			}
		}
	}
	return copied
}

func cloneInt(v *big.Int) *big.Int {
	if v == nil {
		return nil
	}
	return new(big.Int).Set(v)
}
