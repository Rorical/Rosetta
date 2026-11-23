package pipeline

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/graph"
	"github.com/Rorical/Rosetta/internal/pool"
)

type PaperCalibration struct {
	CostModel         string           `json:"cost_model,omitempty"`
	ProtocolCosts     []ProtocolCost   `json:"protocol_costs,omitempty"`
	ConversionPools   []domain.PoolKey `json:"conversion_pools,omitempty"`
	Reason            string           `json:"reason,omitempty"`
	EvidenceHead      domain.BlockRef  `json:"evidence_head"`
	FundingBasis      string           `json:"funding_basis"`
	SampleTransaction domain.Hash      `json:"sample_transaction"`
	SampleBlock       domain.BlockRef  `json:"sample_block"`
	NativeNetworkFee  *big.Int         `json:"native_network_fee,omitempty"`
	FeeSamples        int              `json:"fee_samples"`
	QuoteSamples      int              `json:"minimum_direction_samples"`
	ReservePips       uint64           `json:"quote_reserve_pips"`
	QuoteReserve      *big.Int         `json:"quote_reserve"`
	CalibratedGross   *big.Int         `json:"calibrated_gross"`
	ConversionBasis   string           `json:"conversion_basis,omitempty"`
}

func (b *paperBook) economics(view arb.View, version domain.StateVersion, o arb.Opportunity) ResearchEconomics {
	b.mu.RLock()
	defer b.mu.RUnlock()
	evidence := &PaperCalibration{EvidenceHead: b.head, FundingBasis: b.policy.FundingBasis, QuoteSamples: 64, QuoteReserve: new(big.Int)}
	result := ResearchEconomics{Status: NetUnknown, Basis: "observed_transaction_cost_scenario", Gross: new(big.Int).Set(o.Net), Calibration: evidence}
	unknown := func(reason string) ResearchEconomics { evidence.Reason = reason; return result }
	if view == nil || len(o.Cycle.Pools) < 2 || len(o.Cycle.Pools) > 16 {
		return unknown("missing_quote_state")
	}
	if b.pending || b.head.Hash == (domain.Hash{}) {
		return unknown("awaiting_canonical_cost_samples")
	}
	if version.BlockNumber < b.head.Number || version.Source != domain.SourceStandard && version.BlockNumber == b.head.Number {
		return unknown("cost_evidence_after_quote_position")
	}
	anchor, ok := b.headers[version.Anchor.Number]
	if !ok || anchor.Hash != version.Anchor.Hash {
		return unknown("cost_anchor_not_verified")
	}
	if !b.estimateCost(o.Cycle.Pools, evidence) {
		return unknown("insufficient_protocol_cost_samples")
	}
	wrapped, _ := domain.ParseAddress(b.policy.WrappedNative)
	var pools []pool.State
	var tokens []domain.Address
	token := o.Cycle.Start
	for _, key := range o.Cycle.Pools {
		p, ok := view.Pool(key)
		if !ok {
			return unknown("missing_quote_pool")
		}
		pools = append(pools, p)
		tokens = append(tokens, token)
		samples := b.quotes[quoteDirection{key, token}]
		evidence.QuoteSamples = min(evidence.QuoteSamples, len(samples))
		var worst uint32
		for _, s := range samples {
			worst = max(worst, s.pips)
		}
		evidence.ReservePips += uint64(worst)
		if token == p.Token0 {
			token = p.Token1
		} else if token == p.Token1 {
			token = p.Token0
		} else {
			return unknown("invalid_route_tokens")
		}
	}
	if token != o.Cycle.Start || o.Output == nil {
		return unknown("incomplete_route_quote")
	}
	evidence.QuoteReserve = ceilResearch(new(big.Int).Mul(o.Output, new(big.Int).SetUint64(evidence.ReservePips)), 1000000)
	evidence.CalibratedGross = new(big.Int).Sub(o.Net, evidence.QuoteReserve)
	converted, basis, ok := convertNativeFee(pools, tokens, o.Cycle.Start, wrapped, evidence.NativeNetworkFee)
	if !ok {
		converted, evidence.ConversionPools, ok = b.convertOutsideRoute(view, o.Cycle.Start, wrapped, evidence.NativeNetworkFee)
		basis = "local_pool_spot_scenario"
		if !ok {
			return unknown("unsupported_native_conversion")
		}
	}
	evidence.ConversionBasis = basis

	result.NetworkFee = converted
	result.LoanFee = ceilResearch(new(big.Int).Mul(o.Input, new(big.Int).SetUint64(b.policy.LoanFeePips)), 1000000)
	fixed, _ := new(big.Int).SetString(b.policy.LoanFixed, 10)
	result.LoanFee.Add(result.LoanFee, fixed)
	result.Safety = new(big.Int).Set(evidence.QuoteReserve)
	result.Net = new(big.Int).Sub(evidence.CalibratedGross, converted)
	result.Net.Sub(result.Net, result.LoanFee)
	if evidence.QuoteSamples < b.policy.MinSamples {
		return unknown("insufficient_direction_calibration")
	}
	minimum, _ := new(big.Int).SetString(b.policy.MinNetProfit, 10)
	result.classify(minimum)
	if !result.Eligible {
		evidence.Reason = "below_observed_cost_and_quote_reserve"
	}
	return result
}
func paperSpot(p pool.State, token domain.Address) (n, d *big.Int, ok bool) {
	if p.Key.Kind == domain.PoolKindV2 || p.Key.Kind == domain.PoolKindVelodromeV2 {
		if p.Key.Kind == domain.PoolKindVelodromeV2 && p.Velodrome != nil && p.Velodrome.Stable {
			model, err := dex.RankingModel(p)
			if err != nil {
				return nil, nil, false
			}
			n, d = model.Reserve1, model.Reserve0
			if token == p.Token1 {
				n, d = d, n
			} else if token != p.Token0 {
				return nil, nil, false
			}
			return n, d, n != nil && d != nil && n.Sign() > 0 && d.Sign() > 0
		}
		n, d = p.Reserve1, p.Reserve0
	} else if p.Key.Kind == domain.PoolKindMetricOmm {
		// The pool's price is the oracle quote: the middle of bid and ask,
		// token1 per token0 in whole tokens (Q64.64), brought to raw units by
		// the pool's scale multipliers.
		m := p.Metric
		if m == nil || m.Stale || m.BidX64 == nil || m.AskX64 == nil || m.Token0Scale == nil || m.Token1Scale == nil {
			return nil, nil, false
		}
		n = new(big.Int).Mul(new(big.Int).Add(m.BidX64, m.AskX64), m.Token0Scale)
		d = new(big.Int).Lsh(m.Token1Scale, 65)
	} else if p.SqrtPriceX96 != nil {
		n = new(big.Int).Mul(p.SqrtPriceX96, p.SqrtPriceX96)
		d = new(big.Int).Lsh(big.NewInt(1), 192)
	} else {
		return nil, nil, false
	}
	if token == p.Token1 {
		n, d = d, n
	} else if token != p.Token0 {
		return nil, nil, false
	}
	return n, d, n != nil && d != nil && n.Sign() > 0 && d.Sign() > 0
}

// Economics performs no RPC and returns owned numbers before the consumer's
// execution reader is released. An observed transaction is a cost scenario,
// not the fee of a hypothetical future executor call.
func (r *Research) Economics(event arb.SearchEvent, o arb.Opportunity) ResearchEconomics {
	if r == nil {
		return ResearchEconomics{Status: NetUnknown, Basis: "gross_only", Gross: new(big.Int).Set(o.Net)}
	}
	if r.book != nil {
		return r.book.economics(event.Job.View, event.Job.Version, o)
	}
	return r.policy.Economics(o.Cycle.Start, o.Input, o.Net)
}

// NativeFeeInRoute values a paid native expense using the historical route's
// local spot prices. Wrapped native is exact 1:1; other currencies are a
// marked valuation scenario, not a claim that gas was paid in that currency.
func NativeFeeInRoute(view arb.View, cycle graph.Cycle, wrapped domain.Address, native *big.Int) (*big.Int, string, bool) {
	var pools []pool.State
	var tokens []domain.Address
	token := cycle.Start
	for _, key := range cycle.Pools {
		p, ok := view.Pool(key)
		if !ok {
			return nil, "", false
		}
		pools = append(pools, p)
		tokens = append(tokens, token)
		if token == p.Token0 {
			token = p.Token1
		} else if token == p.Token1 {
			token = p.Token0
		} else {
			return nil, "", false
		}
	}
	if token != cycle.Start {
		return nil, "", false
	}
	return convertNativeFee(pools, tokens, cycle.Start, wrapped, native)
}
func convertNativeFee(pools []pool.State, tokens []domain.Address, asset, wrapped domain.Address, native *big.Int) (*big.Int, string, bool) {
	if native == nil || native.Sign() < 0 {
		return nil, "", false
	}
	converted := new(big.Int).Set(native)
	if asset == wrapped {
		return converted, "wrapped_native_1_to_1", true
	}
	index := -1
	for i, t := range tokens {
		if t == wrapped {
			index = i
			break
		}
	}
	if index < 0 {
		return nil, "", false
	}
	for i := index; i < len(pools); i++ {
		n, d, ok := paperSpot(pools[i], tokens[i])
		if !ok {
			return nil, "", false
		}
		product := new(big.Int).Mul(converted, n)
		q, r := new(big.Int), new(big.Int)
		q.QuoRem(product, d, r)
		if r.Sign() > 0 {
			q.Add(q, big.NewInt(1))
		}
		converted = q
	}
	return converted, "route_spot_scenario", true
}
