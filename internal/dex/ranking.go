package dex

import (
	"github.com/Rorical/Rosetta/internal/dex/metricomm"
	"github.com/Rorical/Rosetta/internal/dex/velodromev2"
	"github.com/Rorical/Rosetta/internal/dex/wrap"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// RankingModel keeps protocol math out of the arbitrage scheduler. Models are
// constructed once per pool/snapshot, before parallel path evaluation.
func RankingModel(p pool.State) (pool.RankingModel, error) {
	if p.Key.Kind == domain.PoolKindVelodromeV2 {
		return velodromev2.RankingModel(p)
	}
	if p.Key.Kind == domain.PoolKindMetricOmm {
		return metricomm.RankingModel(p)
	}
	if p.Key.Kind == domain.PoolKindWrap {
		return wrap.RankingModel(p)
	}
	x, y, ok := pool.SpotReserves(p)
	if !ok {
		return pool.RankingModel{}, pool.ErrNoLiquidity
	}
	return pool.RankingModel{Reserve0: x, Reserve1: y, ConstantProduct: true}, nil
}
