package catalog

import (
	"context"
	"fmt"
	"slices"

	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pool"
)

// MetricPool registers one MetricOmmPool by address. The pools are few and
// no factory lookup maps a token pair to them, and the oracle slot and the
// quote extension's record slot cannot be derived from the pool: they were
// read off transaction traces of the deployed pools.
type MetricPool struct {
	Pool   domain.Address
	Oracle domain.Address
	// OracleSlot is the oracle's storage slot of the last update.
	OracleSlot domain.Hash
	// FeedIndex is the position of the pool's token0 in an oracle update.
	FeedIndex uint8
	// QuoteExtension and QuoteExtensionSlot locate the extension's record
	// for this pool.
	QuoteExtension     domain.Address
	QuoteExtensionSlot domain.Hash
	// Reference is the oracle the pool's price provider holds the quote to,
	// by the price of ReferenceBaseFeed over ReferenceQuoteFeed. The zero
	// address registers a pool without one.
	Reference          domain.Address
	ReferenceBaseFeed  uint32
	ReferenceQuoteFeed uint32
}

type metricJSON struct {
	Pool               string `json:"pool"`
	Oracle             string `json:"oracle"`
	OracleSlot         string `json:"oracle_slot"`
	FeedIndex          uint8  `json:"feed_index"`
	QuoteExtension     string `json:"quote_extension"`
	QuoteExtensionSlot string `json:"quote_extension_slot"`
	Reference          string `json:"reference_oracle,omitempty"`
	ReferenceBaseFeed  uint32 `json:"reference_base_feed,omitempty"`
	ReferenceQuoteFeed uint32 `json:"reference_quote_feed,omitempty"`
}

func (m metricJSON) decode() (MetricPool, error) {
	decoded := MetricPool{FeedIndex: m.FeedIndex, ReferenceBaseFeed: m.ReferenceBaseFeed, ReferenceQuoteFeed: m.ReferenceQuoteFeed}
	var err error
	if m.Reference != "" {
		if decoded.Reference, err = domain.ParseAddress(m.Reference); err != nil {
			return MetricPool{}, err
		}
	}
	for _, field := range []struct {
		text string
		out  *domain.Address
	}{{m.Pool, &decoded.Pool}, {m.Oracle, &decoded.Oracle}, {m.QuoteExtension, &decoded.QuoteExtension}} {
		if *field.out, err = domain.ParseAddress(field.text); err != nil {
			return MetricPool{}, err
		}
	}
	if decoded.OracleSlot, err = domain.ParseHash(m.OracleSlot); err != nil {
		return MetricPool{}, err
	}
	if decoded.QuoteExtensionSlot, err = domain.ParseHash(m.QuoteExtensionSlot); err != nil {
		return MetricPool{}, err
	}
	return decoded, nil
}

func validateMetric(pools []MetricPool) error {
	if len(pools) > 16 {
		return fmt.Errorf("catalog: too many MetricOmm pools")
	}
	seen := make(map[domain.Address]bool)
	for _, p := range pools {
		if p.Pool == (domain.Address{}) || p.Oracle == (domain.Address{}) || p.QuoteExtension == (domain.Address{}) || seen[p.Pool] {
			return fmt.Errorf("catalog: duplicate or incomplete MetricOmm pool")
		}
		if p.OracleSlot == (domain.Hash{}) || p.QuoteExtensionSlot == (domain.Hash{}) || int(p.FeedIndex) >= metricomm.OracleFeedCount {
			return fmt.Errorf("catalog: MetricOmm pool %s has an invalid slot or feed index", p.Pool.Hex())
		}
		if (p.Reference == (domain.Address{})) != (p.ReferenceBaseFeed == 0 && p.ReferenceQuoteFeed == 0) || p.Reference != (domain.Address{}) && (p.ReferenceBaseFeed == 0 || p.ReferenceQuoteFeed == 0 || p.ReferenceBaseFeed == p.ReferenceQuoteFeed) {
			return fmt.Errorf("catalog: MetricOmm pool %s has an incomplete reference oracle", p.Pool.Hex())
		}
		seen[p.Pool] = true
	}
	return nil
}

// resolveMetric reads each pool's immutables. Both tokens must be in the
// catalog's universe and the configured quote extension must be one the pool
// lists, so a registration cannot bring in an unvetted token or read another
// pool's extension record.
func resolveMetric(ctx context.Context, reader execution.StateReader, pools []MetricPool, tokens []domain.Address) ([]bootstrap.Spec, error) {
	specs := make([]bootstrap.Spec, 0, len(pools))
	for _, p := range pools {
		contracts := []domain.Address{p.Pool, p.Oracle, p.QuoteExtension}
		if p.Reference != (domain.Address{}) {
			contracts = append(contracts, p.Reference)
		}
		for _, contract := range contracts {
			if err := checkCode(ctx, reader, contract); err != nil {
				return nil, err
			}
		}
		output, err := reader.ReadContract(ctx, p.Pool, metricomm.ImmutablesCall())
		if err != nil {
			return nil, fmt.Errorf("catalog: MetricOmm pool %s: %w", p.Pool.Hex(), err)
		}
		registration := pool.MetricState{Oracle: p.Oracle, OracleSlot: p.OracleSlot, FeedIndex: p.FeedIndex, QuoteExtension: p.QuoteExtension, QuoteExtensionSlot: p.QuoteExtensionSlot,
			Reference: p.Reference, ReferenceBaseFeed: p.ReferenceBaseFeed, ReferenceQuoteFeed: p.ReferenceQuoteFeed}
		metric, token0, token1, err := metricomm.Register(registration, output)
		if err != nil {
			return nil, fmt.Errorf("catalog: MetricOmm pool %s: %w", p.Pool.Hex(), err)
		}
		if token0 == (domain.Address{}) || !slices.Contains(tokens, token0) || !slices.Contains(tokens, token1) {
			return nil, fmt.Errorf("catalog: MetricOmm pool %s trades a token outside the catalog", p.Pool.Hex())
		}
		specs = append(specs, bootstrap.Spec{Address: p.Pool, Kind: domain.PoolKindMetricOmm, Token0: token0, Token1: token1, Metric: &metric})
	}
	return specs, nil
}
