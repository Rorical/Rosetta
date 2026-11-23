package pool

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	poolmath "github.com/Rorical/Rosetta/internal/math"
)

// SpotReserves returns real CP reserves or in-range virtual CL reserves.
// Protocols with other curves supply their own RankingModel instead.
func SpotReserves(p State) (*big.Int, *big.Int, bool) {
	if p.Key.Kind == domain.PoolKindV2 {
		return p.Reserve0, p.Reserve1, positive(p.Reserve0) && positive(p.Reserve1)
	}

	if !p.Key.Kind.UsesTicks() || !positive(p.Liquidity) || !positive(p.SqrtPriceX96) {
		return nil, nil, false
	}
	x := new(big.Int).Quo(new(big.Int).Mul(p.Liquidity, poolmath.Q96), p.SqrtPriceX96)
	y := new(big.Int).Quo(new(big.Int).Mul(p.Liquidity, p.SqrtPriceX96), poolmath.Q96)
	return x, y, x.Sign() > 0 && y.Sign() > 0
}

func positive(v *big.Int) bool { return v != nil && v.Sign() > 0 }
