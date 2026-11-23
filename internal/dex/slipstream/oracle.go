package slipstream

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/pool"
)

// wrapTickCumulative matches the oracle's signed int56 accumulator.
func wrapTickCumulative(n int64) int64 { return n << 8 >> 8 }

func cumulativeAt(m *pool.SlipstreamState, now, ago uint32, tick int32) (int64, bool) {
	h := m.Observations
	if h.Len() == 0 {
		return 0, false
	}
	target := now - ago
	last := h.At(h.Len() - 1)
	if now-last.Timestamp >= ago {
		return wrapTickCumulative(last.TickCumulative + int64(tick)*int64(target-last.Timestamp)), true
	}
	// Entries are chronological across uint32 timestamp rollover. Age ordering
	// avoids treating the wrapped timestamp as an earlier observation.
	lo, hi := 0, h.Len()-1
	if now-h.At(0).Timestamp < ago {
		return 0, false
	}
	for lo+1 < hi {
		mid := (lo + hi) / 2
		if now-h.At(mid).Timestamp >= ago {
			lo = mid
		} else {
			hi = mid
		}
	}
	before, after := h.At(lo), h.At(hi)
	if target == before.Timestamp {
		return before.TickCumulative, true
	}
	elapsed := after.Timestamp - before.Timestamp
	if elapsed == 0 {
		return 0, false
	}
	// Oracle divides before multiplying. Interpolation must retain that order.
	delta := wrapTickCumulative(after.TickCumulative - before.TickCumulative)
	return wrapTickCumulative(before.TickCumulative + delta/int64(elapsed)*int64(target-before.Timestamp)), true
}

func writeObservation(m *pool.SlipstreamState, tick int32) {
	h := m.Observations
	if h.Len() == 0 {
		m.Valid = false
		m.NeedsRefresh = true
		m.UnavailableReason = "no_observations"
		return
	}
	last := h.At(h.Len() - 1)
	if last.Timestamp == m.Timestamp {
		return
	}
	if m.CardinalityNext > m.Cardinality && m.ObservationIndex == m.Cardinality-1 {
		m.Cardinality = m.CardinalityNext
	}
	index := uint16((uint32(m.ObservationIndex) + 1) % uint32(m.Cardinality))
	next := pool.OracleObservation{Index: index, Timestamp: m.Timestamp, TickCumulative: wrapTickCumulative(last.TickCumulative + int64(tick)*int64(m.Timestamp-last.Timestamp))}
	start := 0
	// Drop overwritten slots and history older than the interpolation boundary.
	for start < h.Len() && h.At(start).Index == index {
		start++
	}
	for start+1 < h.Len() && m.Timestamp-h.At(start+1).Timestamp >= m.SecondsAgo {
		start++
		m.Complete = false
	}
	m.Observations = h.AppendFrom(start, next)
	m.ObservationIndex = index
}

// ValidateState checks persisted model boundaries before exposing a snapshot.
func ValidateState(m *pool.SlipstreamState) error {
	if m == nil {
		return nil
	}
	if !m.Valid {
		return nil
	}
	maximumLookback := uint32(65535)
	if m.Legacy {
		maximumLookback *= 2
	}
	if m.Legacy && (m.SecondsAgo < 2 || m.BaseFee > 30000 || m.BlockFee != 0 || m.InitialFeeEnabled) {
		return fmt.Errorf("%w: legacy fee configuration", pool.ErrInvalidState)
	}
	if m.Cardinality == 0 || m.CardinalityNext < m.Cardinality || m.ObservationIndex >= m.Cardinality || m.SecondsAgo == 0 || m.SecondsAgo >= maximumLookback || m.Observations.Len() == 0 || m.Observations.Len() > int(m.Cardinality) {
		return fmt.Errorf("%w: invalid oracle metadata", pool.ErrInvalidState)
	}
	if m.BaseFee > 50000 && m.BaseFee != 0xffffff || m.InitialFee > 50000 && m.InitialFee != 0xffffff || m.FeeCap > 50000 || (!m.Legacy && m.DefaultFeeCap == 0) || m.DefaultFeeCap > 50000 || m.ScalingFactor > 1e18 || m.DefaultScalingFactor > 1e18 || m.SpacingFee >= 1_000_000 || m.BlockFee > 100000 {
		return fmt.Errorf("%w: invalid dynamic fee configuration", pool.ErrInvalidState)
	}
	for i := 0; i < m.Observations.Len(); i++ {
		o := m.Observations.At(i)
		if o.Index >= m.Cardinality || o.TickCumulative != wrapTickCumulative(o.TickCumulative) {
			return fmt.Errorf("%w: invalid observation", pool.ErrInvalidState)
		}
		if i > 0 && m.Timestamp-m.Observations.At(i-1).Timestamp <= m.Timestamp-o.Timestamp {
			return fmt.Errorf("%w: unordered observations", pool.ErrInvalidState)
		}
	}
	if m.Observations.At(m.Observations.Len()-1).Index != m.ObservationIndex {
		return fmt.Errorf("%w: latest observation index", pool.ErrInvalidState)
	}
	return nil
}
