// Package velodromev2 implements the OP Velodrome V2 Pool deployment.
package velodromev2

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

var precision = big.NewInt(1_000_000_000_000_000_000)

type Adapter struct{}

func New() *Adapter                    { return &Adapter{} }
func (*Adapter) Kind() domain.PoolKind { return domain.PoolKindVelodromeV2 }

// QuoteExactIn follows the deployed Pool's integer operations. In particular,
// fees are rounded down and removed before pricing, unlike Uniswap V2's ratio.
func (*Adapter) QuoteExactIn(ctx context.Context, p pool.State, _ pool.DeepState, token domain.Address, amount *big.Int) (*big.Int, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validate(p); err != nil {
		return nil, err
	}
	if amount == nil || amount.Sign() <= 0 || amount.BitLen() > 256 {
		return nil, fmt.Errorf("%w: Velodrome input amount", pool.ErrInvalidState)
	}
	rIn, rOut := p.Reserve0, p.Reserve1
	sIn, sOut := p.Velodrome.Scale0, p.Velodrome.Scale1
	if token == p.Token1 {
		rIn, rOut, sIn, sOut = rOut, rIn, sOut, sIn
	} else if token != p.Token0 {
		return nil, fmt.Errorf("%w: Velodrome input token", pool.ErrInvalidState)
	}
	c := arithmetic{}
	fee := c.mulDiv(amount, new(big.Int).SetUint64(uint64(p.Fee/100)), big.NewInt(10000))
	net := new(big.Int).Sub(amount, fee)
	if !p.Velodrome.Stable {
		out := c.mulDiv(net, rOut, new(big.Int).Add(rIn, net))
		return out, c.err
	}
	x, y := c.mulDiv(rIn, precision, sIn), c.mulDiv(rOut, precision, sOut)
	target := c.f(x, y)
	newX := new(big.Int).Add(x, c.mulDiv(net, precision, sIn))
	remaining, err := solveY(ctx, &c, p.Velodrome, newX, target, y)
	if err != nil {
		return nil, err
	}
	if remaining.Cmp(y) > 0 {
		return nil, fmt.Errorf("%w: Velodrome negative output", pool.ErrInvalidState)
	}
	out := c.mulDiv(new(big.Int).Sub(y, remaining), sOut, precision)
	return out, c.err
}

// supportedFactory admits the two deployments with verified arithmetic: the V2
// PoolFactory and the V1 PairFactory, whose pairs share V2's invariants
// (testdata/v1_quotes.json).
func supportedFactory(m *pool.VelodromeState) bool {
	return m.Factory == factory || m.Factory == legacyFactory
}

func validate(p pool.State) error {
	m := p.Velodrome
	if p.Key.Kind != domain.PoolKindVelodromeV2 || m == nil || !supportedFactory(m) || p.Fee > 10000 || p.Fee%100 != 0 ||
		p.Token0 == p.Token1 || p.Reserve0 == nil || p.Reserve1 == nil ||
		m.Scale0 == nil || m.Scale1 == nil || m.Scale0.Sign() <= 0 || m.Scale1.Sign() <= 0 {
		return fmt.Errorf("%w: Velodrome metadata", pool.ErrInvalidState)
	}
	if p.Reserve0.Sign() <= 0 || p.Reserve1.Sign() <= 0 {
		return pool.ErrNoLiquidity
	}
	return nil
}

// solveY reproduces implementation 0x95885Af5492195F0754bE71AD1545Fe81364E531.
// Its upward zero-step check calls _k (normalizing again), whereas newer Pool
// versions call _f. Substituting the newer formula changes deployed rounding.
func solveY(ctx context.Context, c *arithmetic, m *pool.VelodromeState, x, target, initial *big.Int) (*big.Int, error) {
	y := new(big.Int).Set(initial)
	one := big.NewInt(1)
	for i := 0; i < 255; i++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		k := c.f(x, y)
		d := c.derivative(x, y)
		if c.err != nil {
			return nil, c.err
		}
		if d.Sign() == 0 {
			return nil, fmt.Errorf("%w: Velodrome zero derivative", pool.ErrInvalidState)
		}
		if k.Cmp(target) < 0 {
			dy := c.mulDiv(new(big.Int).Sub(target, k), precision, d)
			if dy.Sign() == 0 {
				next := new(big.Int).Add(y, one)
				check := c.f(c.mulDiv(x, precision, m.Scale0), c.mulDiv(next, precision, m.Scale1))
				if c.err != nil {
					return nil, c.err
				}
				if check.Cmp(target) > 0 {
					return next, nil
				}
				dy.SetInt64(1)
			}
			y.Add(y, dy)
		} else {
			dy := c.mulDiv(new(big.Int).Sub(k, target), precision, d)
			if dy.Sign() == 0 {
				if k.Cmp(target) == 0 || (y.Sign() > 0 && c.f(x, new(big.Int).Sub(y, one)).Cmp(target) < 0) {
					return y, c.err
				}
				dy.SetInt64(1)
			}
			if dy.Cmp(y) > 0 {
				return nil, fmt.Errorf("%w: Velodrome Newton underflow", pool.ErrInvalidState)
			}
			y.Sub(y, dy)
		}
	}
	return nil, fmt.Errorf("%w: Velodrome Newton did not converge", pool.ErrInvalidState)
}

// Reject Solidity uint256 intermediate overflows rather than quoting swaps the
// deployed contract would revert. The sticky error keeps the formulas readable.
type arithmetic struct{ err error }

func (c *arithmetic) mulDiv(a, b, divisor *big.Int) *big.Int {
	v := new(big.Int).Mul(a, b)
	if c.err != nil {
		return new(big.Int)
	}
	if a.Sign() < 0 || b.Sign() < 0 || a.BitLen() > 256 || b.BitLen() > 256 || v.BitLen() > 256 || divisor.Sign() <= 0 || divisor.BitLen() > 256 {
		c.err = fmt.Errorf("%w: Velodrome uint256 arithmetic", pool.ErrInvalidState)
		return new(big.Int)
	}
	return v.Quo(v, divisor)
}
func (c *arithmetic) f(x, y *big.Int) *big.Int {
	a := c.mulDiv(x, y, precision)
	b := new(big.Int).Add(c.mulDiv(x, x, precision), c.mulDiv(y, y, precision))
	return c.mulDiv(a, b, precision)
}
func (c *arithmetic) derivative(x, y *big.Int) *big.Int {
	a := c.mulDiv(new(big.Int).Mul(big.NewInt(3), x), c.mulDiv(y, y, precision), precision)
	b := c.mulDiv(c.mulDiv(x, x, precision), x, precision)
	return new(big.Int).Add(a, b)
}
