package velodromev2

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/pool"
)

// RankingModel uses the stable invariant derivative, not the reserve ratio.
// Synthetic reserves suggest ordering only; they are not CP rejection proofs.
// Volatile fees also round differently from the Uniswap CP screening model.
func RankingModel(p pool.State) (pool.RankingModel, error) {
	if err := validate(p); err != nil {
		return pool.RankingModel{}, err
	}
	if !p.Velodrome.Stable {
		return pool.RankingModel{Reserve0: p.Reserve0, Reserve1: p.Reserve1}, nil
	}
	m := p.Velodrome
	x := new(big.Int).Quo(new(big.Int).Mul(p.Reserve0, precision), m.Scale0)
	y := new(big.Int).Quo(new(big.Int).Mul(p.Reserve1, precision), m.Scale1)
	if x.Sign() == 0 || y.Sign() == 0 {
		return pool.RankingModel{}, pool.ErrNoLiquidity
	}
	x2, y2 := new(big.Int).Mul(x, x), new(big.Int).Mul(y, y)
	a := new(big.Int).Add(x2, new(big.Int).Mul(big.NewInt(3), y2))
	b := new(big.Int).Add(new(big.Int).Mul(big.NewInt(3), x2), y2)
	// dy/dx = y*b/(x*a). Fix each curve's output capacity to the actual
	// output inventory and solve its input reserve from that derivative.
	// Unnormalized derivative numerators have cubic units and previously
	// made price impact almost zero, even for reserve-sized swaps.
	forward := new(big.Int).Quo(new(big.Int).Mul(new(big.Int).Mul(x, a), m.Scale0), new(big.Int).Mul(b, precision))
	reverse := new(big.Int).Quo(new(big.Int).Mul(new(big.Int).Mul(y, b), m.Scale1), new(big.Int).Mul(a, precision))
	if forward.Sign() == 0 || reverse.Sign() == 0 {
		return pool.RankingModel{}, pool.ErrNoLiquidity
	}
	return pool.RankingModel{Reserve0: forward, Reserve1: p.Reserve1, ReverseIn: reverse, ReverseOut: p.Reserve0}, nil
}
