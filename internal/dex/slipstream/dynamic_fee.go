package slipstream

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// LocalFee is the supported hook's fee for a new zero-tip transaction without
// discounts. Unknown or invalidated oracle state cannot masquerade as a quote.
func LocalFee(p pool.State) (uint32, error) {
	m := p.Slipstream
	if m == nil {
		return p.Fee, nil
	}
	if !m.Valid {
		return 0, fmt.Errorf("%w: Slipstream local fee state requires refresh", pool.ErrInvalidState)
	}
	if m.Legacy {
		return firstBlockFee(p)
	}
	raw := m.BlockFee
	if raw == 0 {
		var err error
		raw, err = firstBlockFee(p)
		if err != nil {
			return 0, err
		}
		if m.BaseFee == 0xffffff {
			return m.SpacingFee, nil
		}
		if m.InitialFeeEnabled {
			switch m.InitialFee {
			case 0:
				raw = m.BaseFee
				if raw == 0 {
					raw = m.SpacingFee
				}
			case 0xffffff:
				raw = m.SpacingFee
			default:
				raw = m.InitialFee
			}
		}
	}
	if raw == 0 {
		return m.SpacingFee, nil
	}
	return min(raw, 100000), nil
}

func firstBlockFee(p pool.State) (uint32, error) {
	m := p.Slipstream
	if m.Legacy && m.BaseFee == 420 {
		return 0, nil
	}
	if m.BaseFee == 0xffffff {
		return m.SpacingFee, nil
	}
	base := m.BaseFee
	if base == 0 {
		base = m.SpacingFee
	}
	cap, factor := m.FeeCap, m.ScalingFactor
	if factor == 0 {
		cap, factor = m.DefaultFeeCap, m.DefaultScalingFactor
	}
	if m.Legacy && uint32(m.Cardinality) < m.SecondsAgo/2 {
		return min(base, cap), nil
	}
	old, ok := cumulativeAt(m, m.Timestamp, m.SecondsAgo, p.Tick)
	if !ok {
		if !m.Complete {
			return 0, fmt.Errorf("%w: Slipstream oracle lookback unavailable", pool.ErrInvalidState)
		}
		return min(base, cap), nil
	}
	current, _ := cumulativeAt(m, m.Timestamp, 0, p.Tick)
	delta := current - old
	if m.Legacy {
		delta = wrapTickCumulative(delta)
	}
	if delta != wrapTickCumulative(delta) {
		return 0, fmt.Errorf("%w: hook int56 subtraction overflow", pool.ErrInvalidState)
	}
	twap := delta / int64(m.SecondsAgo)
	if !m.Legacy && delta < 0 && delta%int64(m.SecondsAgo) != 0 {
		twap--
	}
	difference := int64(p.Tick) - twap
	if difference < 0 {
		difference = -difference
	}
	// Saturate at the cap before multiplication; the configured factor can be
	// 1e18 and a full-range tick difference would overflow uint64.
	if base >= cap {
		return cap, nil
	}
	remaining := uint64(cap-base) * 1_000_000
	if difference != 0 && factor > remaining/uint64(difference) {
		return cap, nil
	}
	return min(base+uint32(uint64(difference)*factor/1_000_000), cap), nil
}

// TransactionFee applies caller/origin's resolved discount and priority fee
// tax. The default search uses neither; execution can supply its exact context.
func TransactionFee(p pool.State, baseFee, priorityFee *big.Int, discount uint32) (uint32, error) {
	fee, err := LocalFee(p)
	if err != nil {
		return 0, err
	}
	m := p.Slipstream
	if discount > 1_000_000 || baseFee == nil || priorityFee == nil || baseFee.Sign() < 0 || priorityFee.Sign() < 0 {
		return 0, fmt.Errorf("%w: Slipstream transaction fee context", pool.ErrInvalidState)
	}
	if m == nil {
		// A deployment without a local model, its fee read each block. With
		// no tip and no discount no tip-dependent tax applies, so the read fee
		// is charged: on Robinhood's Aero WETH/USDG pool in-tick swaps pay
		// fee() of their block. An unknown tax module may
		// price a tip, so a tip or discount is refused.
		if priorityFee.Sign() != 0 || discount != 0 {
			return 0, fmt.Errorf("%w: unmodelled Slipstream pool with a tip or discount", pool.ErrInvalidState)
		}
		return fee, nil
	}
	if m.Legacy {
		if discount > 500000 {
			return 0, pool.ErrInvalidState
		}
		return uint32(uint64(fee) * uint64(1_000_000-discount) / 1_000_000), nil
	}
	initial := m.BlockFee == 0 && (m.InitialFeeEnabled || m.BaseFee == 0xffffff)
	if !initial && discount > 0 {
		fee = uint32((uint64(fee)*uint64(1_000_000-discount) + 999999) / 1_000_000)
	}
	if m.TaxModule != (domain.Address{}) && priorityFee.Sign() > 0 {
		minimum, ok := new(big.Int).SetString(m.TaxMinimum, 10)
		if !ok || minimum.Sign() < 0 || minimum.BitLen() > 96 {
			return 0, pool.ErrInvalidState
		}
		factor, ok := new(big.Int).SetString(m.TaxBaseFactor, 10)
		if !ok || factor.Sign() < 0 || factor.BitLen() > 96 {
			return 0, pool.ErrInvalidState
		}
		threshold := new(big.Int).Mul(factor, baseFee)
		if minimum.Cmp(threshold) > 0 {
			threshold = minimum
		}
		if priorityFee.Cmp(threshold) > 0 {
			tax := new(big.Int).Sub(priorityFee, threshold)
			tax.Mul(tax, new(big.Int).SetUint64(m.TaxMultiplier))
			tax.Add(tax, big.NewInt(999999999999))
			tax.Quo(tax, big.NewInt(1_000_000_000_000))
			if tax.Cmp(big.NewInt(100000)) > 0 {
				tax.SetUint64(100000)
			}
			fee += uint32(tax.Uint64())
		}
	}
	if fee == 0 {
		fee = m.SpacingFee
	}
	return min(fee, 100000), nil
}

// AdvanceTime recalculates fees even when a block has no pool Swap log.
func AdvanceTime(p *pool.State, ref domain.BlockRef) {
	m := p.Slipstream
	if m == nil || !m.Valid {
		return
	}
	if m.Block != ref.Number {
		m.BlockFee = 0
	}
	m.Block, m.Timestamp = ref.Number, uint32(ref.Timestamp)
	refreshLocalFee(p)
}

// FollowEvent runs before the V3 price reducer. The hook caches the first
// pre-swap fee, and the oracle writes the previous tick, at most once per time.
func FollowEvent(p *pool.State, ev uniswapv3.Event) {
	m := p.Slipstream
	if m == nil || !m.Valid {
		return
	}
	switch ev.Kind {
	case uniswapv3.EventSwap:
		if !m.Legacy && m.BlockFee == 0 {
			fee, err := firstBlockFee(*p)
			if err != nil {
				m.Valid = false
				m.NeedsRefresh = true
				m.UnavailableReason = "first_block_fee: " + err.Error()
				return
			}
			m.BlockFee = fee
		}
		if ev.Tick != p.Tick {
			writeObservation(m, p.Tick)
		}
	case uniswapv3.EventMint, uniswapv3.EventBurn:
		if ev.Amount != nil && ev.Amount.Sign() != 0 && ev.TickLower <= p.Tick && p.Tick < ev.TickUpper {
			writeObservation(m, p.Tick)
		}
	case uniswapv3.EventInitialize:
		m.Valid = false
		m.NeedsRefresh = true
		m.UnavailableReason = "initialized"
	}
}

func RefreshFee(p *pool.State) { refreshLocalFee(p) }
func refreshLocalFee(p *pool.State) {
	if p.Slipstream == nil || !p.Slipstream.Valid {
		return
	}
	fee, err := LocalFee(*p)
	if err != nil {
		p.Slipstream.Valid = false
		p.Slipstream.NeedsRefresh = true
		p.Slipstream.UnavailableReason = err.Error()
		return
	}
	p.Fee = fee
}
