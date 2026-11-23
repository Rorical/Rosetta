package pipeline

import (
	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// RiskRanking checks the current token and pool exclusion sets on each pool
// snapshot. Newly observed risks invalidate numerical ranking inputs without
// RPC on search.
func RiskRanking(tokens func(domain.Address) error, pools func(domain.PoolKey) error) func(pool.State) (pool.RankingModel, error) {
	return func(p pool.State) (pool.RankingModel, error) {
		if err := CheckPoolRisk(p, tokens, pools); err != nil {
			return pool.RankingModel{}, err
		}
		return dex.RankingModel(p)
	}
}

// CheckPoolRisk rejects a quarantined pool or one holding an excluded token.
func CheckPoolRisk(p pool.State, tokens func(domain.Address) error, pools func(domain.PoolKey) error) error {
	if pools != nil {
		if err := pools(p.Key); err != nil {
			return err
		}
	}
	return CheckPoolTokens(p, tokens)
}

func CheckPoolTokens(p pool.State, check func(domain.Address) error) error {
	if check == nil {
		return nil
	}
	if err := check(p.Token0); err != nil {
		return err
	}
	return check(p.Token1)
}

func filterRiskPools(pools []pool.State, tokens func(domain.Address) error, quarantined func(domain.PoolKey) error) []pool.State {
	if tokens == nil && quarantined == nil {
		return pools
	}
	filtered := make([]pool.State, 0, len(pools))
	for _, p := range pools {
		if CheckPoolRisk(p, tokens, quarantined) == nil {
			filtered = append(filtered, p)
		}
	}
	return filtered
}
