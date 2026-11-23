package state

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// metricWire is a MetricOmm pool's registration. The storage words, the
// extension state and the quote are left out on purpose: bins move with every
// swap and the quote every two seconds, so a restored copy would be wrong by
// the time it is read. The pool comes back stale and the runtime reads it at
// its first canonical block.
type metricWire struct {
	Token0Scale        string `json:"token0_scale"`
	Token1Scale        string `json:"token1_scale"`
	LowestBin          int16  `json:"lowest_bin"`
	HighestBin         int16  `json:"highest_bin"`
	Oracle             string `json:"oracle"`
	OracleSlot         string `json:"oracle_slot"`
	FeedIndex          uint8  `json:"feed_index"`
	QuoteExtension     string `json:"quote_extension"`
	QuoteExtensionSlot string `json:"quote_extension_slot"`
	Reference          string `json:"reference_oracle,omitempty"`
	ReferenceBaseFeed  uint32 `json:"reference_base_feed,omitempty"`
	ReferenceQuoteFeed uint32 `json:"reference_quote_feed,omitempty"`
}

func metricToWire(m *pool.MetricState) *metricWire {
	if m == nil {
		return nil
	}
	reference := ""
	if m.Reference != (domain.Address{}) {
		reference = m.Reference.Hex()
	}
	return &metricWire{
		Reference: reference, ReferenceBaseFeed: m.ReferenceBaseFeed, ReferenceQuoteFeed: m.ReferenceQuoteFeed,
		Token0Scale: decimalString(m.Token0Scale), Token1Scale: decimalString(m.Token1Scale),
		LowestBin: m.LowestBin, HighestBin: m.HighestBin,
		Oracle: m.Oracle.Hex(), OracleSlot: m.OracleSlot.Hex(), FeedIndex: m.FeedIndex,
		QuoteExtension: m.QuoteExtension.Hex(), QuoteExtensionSlot: m.QuoteExtensionSlot.Hex(),
	}
}

func metricFromWire(wire *metricWire) (*pool.MetricState, error) {
	if wire == nil {
		return nil, nil
	}
	m := &pool.MetricState{LowestBin: wire.LowestBin, HighestBin: wire.HighestBin, FeedIndex: wire.FeedIndex, Stale: true,
		ReferenceBaseFeed: wire.ReferenceBaseFeed, ReferenceQuoteFeed: wire.ReferenceQuoteFeed}
	var ok bool
	if m.Token0Scale, ok = new(big.Int).SetString(wire.Token0Scale, 10); !ok || m.Token0Scale.Sign() <= 0 {
		return nil, fmt.Errorf("invalid MetricOmm token0 scale %q", wire.Token0Scale)
	}
	if m.Token1Scale, ok = new(big.Int).SetString(wire.Token1Scale, 10); !ok || m.Token1Scale.Sign() <= 0 {
		return nil, fmt.Errorf("invalid MetricOmm token1 scale %q", wire.Token1Scale)
	}
	if m.HighestBin < m.LowestBin {
		return nil, fmt.Errorf("invalid MetricOmm bin grid %d..%d", m.LowestBin, m.HighestBin)
	}
	var err error
	if m.Oracle, err = domain.ParseAddress(wire.Oracle); err != nil {
		return nil, err
	}
	if m.QuoteExtension, err = domain.ParseAddress(wire.QuoteExtension); err != nil {
		return nil, err
	}
	if wire.Reference != "" {
		if m.Reference, err = domain.ParseAddress(wire.Reference); err != nil {
			return nil, err
		}
	}
	if m.OracleSlot, err = domain.ParseHash(wire.OracleSlot); err != nil {
		return nil, err
	}
	if m.QuoteExtensionSlot, err = domain.ParseHash(wire.QuoteExtensionSlot); err != nil {
		return nil, err
	}
	return m, nil
}
