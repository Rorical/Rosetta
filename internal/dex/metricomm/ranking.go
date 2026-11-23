package metricomm

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// RankingModel is the pool's screening approximation: per direction a
// product curve fitted to two exact quotes, a small one for the spot rate
// and one for about a fiftieth of the output inventory for the slope. The
// pool's price is the quote moved by the current bin's distance and changes
// by one bin length per bin of liquidity, so a curve over the whole
// inventory at the bid or ask is wrong in both level and slope (it models
// about 35 bps of slippage on a $200 trade, so no route through the pool
// would rank). It is not a constant-product pool; exact amounts
// come from the adapter, which also bounds a trade the pool cannot fill.
//
// A stale pool, one without a quote, or one that is paused or empty on
// either side has no model and so no edge.
func RankingModel(p pool.State) (pool.RankingModel, error) {
	metric := p.Metric
	if metric == nil {
		return pool.RankingModel{}, fmt.Errorf("%w: no MetricOmm state", pool.ErrInvalidState)
	}
	if metric.Stale {
		return pool.RankingModel{}, ErrStaleState
	}
	if metric.BidX64 == nil || metric.AskX64 == nil {
		return pool.RankingModel{}, ErrNoQuote
	}
	state, err := decodeMetricState(metric)
	if err != nil {
		return pool.RankingModel{}, err
	}
	if state.PauseLevel != 0 {
		return pool.RankingModel{}, ErrPoolPaused
	}
	// Balances are kept scaled; a quote is token1 per token0 in scaled units.
	held0 := new(big.Int).Quo(state.TotalToken0Scaled.ToBig(), metric.Token0Scale)
	held1 := new(big.Int).Quo(state.TotalToken1Scaled.ToBig(), metric.Token1Scale)
	// token0 in at the bid: in = held1 * 2^64 * scale1 / (bid * scale0).
	sellAll := new(big.Int).Lsh(new(big.Int).Mul(held1, metric.Token1Scale), 64)
	sellAll.Quo(sellAll, new(big.Int).Mul(metric.BidX64, metric.Token0Scale))
	// token1 in at the ask: in = held0 * ask * scale0 / (2^64 * scale1).
	buyAll := new(big.Int).Mul(new(big.Int).Mul(held0, metric.AskX64), metric.Token0Scale)
	buyAll.Quo(buyAll, new(big.Int).Lsh(metric.Token1Scale, 64))
	if held0.Sign() <= 0 || held1.Sign() <= 0 || sellAll.Sign() <= 0 || buyAll.Sign() <= 0 {
		return pool.RankingModel{}, pool.ErrNoLiquidity
	}
	reference := new(big.Int).Mul(metric.BidX64, metric.AskX64)
	reference.Sqrt(reference)
	sellIn, sellOut, err := fitCurve(state, true, sellAll, metric.BidX64, metric.AskX64, reference)
	if err != nil {
		return pool.RankingModel{}, err
	}
	buyIn, buyOut, err := fitCurve(state, false, buyAll, metric.BidX64, metric.AskX64, reference)
	if err != nil {
		return pool.RankingModel{}, err
	}
	return pool.RankingModel{Reserve0: sellIn, Reserve1: sellOut, ReverseIn: buyIn, ReverseOut: buyOut}, nil
}

const (
	// rankSpotShare and rankSlopeShare are the shares of the input that
	// would empty the output side at which the curve is fitted.
	// A smaller spot trade rounds too coarsely in an 8-decimal token: a
	// ten-thousandth of the WBTC pool is 1,575 units, 6 bps a unit.
	rankSpotShare  = 1_000
	rankSlopeShare = 50
	// rankFlatReserve stands in for an input reserve when the two quotes
	// show no slippage at all.
	rankFlatReserveBits = 200
)

// fitCurve returns the input and output reserves of the product curve
// out = in*Out/(In+in) through two exact quotes of the pool in one direction;
// all is the input that would buy the whole output side at the quote.
func fitCurve(state State, zeroForOne bool, all, bid, ask, reference *big.Int) (reserveIn, reserveOut *big.Int, err error) {
	quote := func(share int64) (*big.Int, *big.Int, bool) {
		amount := new(big.Int).Quo(all, big.NewInt(share))
		if amount.Sign() <= 0 {
			return nil, nil, false
		}
		limit := noBuyPriceLimit
		if zeroForOne {
			limit = new(big.Int)
		}
		amount0, amount1, _, err := Swap(state, zeroForOne, amount, limit, bid, ask, reference)
		if err != nil {
			return nil, nil, false
		}
		consumed, received := amount0, amount1
		if !zeroForOne {
			consumed, received = amount1, amount0
		}
		if consumed.Cmp(amount) != 0 || received.Sign() >= 0 {
			return nil, nil, false
		}
		return amount, received.Neg(received), true
	}
	small, smallOut, ok := quote(rankSpotShare)
	if !ok {
		return nil, nil, pool.ErrNoLiquidity
	}
	// The slope point halves until the pool fills it whole.
	var large, largeOut *big.Int
	for share := int64(rankSlopeShare); share < rankSpotShare; share *= 2 {
		if large, largeOut, ok = quote(share); ok {
			break
		}
	}
	if !ok {
		return nil, nil, pool.ErrNoLiquidity
	}
	// Spot rate smallOut/small; In = large*largeOut / (large*spot - largeOut).
	numerator := new(big.Int).Mul(new(big.Int).Mul(large, largeOut), small)
	denominator := new(big.Int).Mul(large, smallOut)
	denominator.Sub(denominator, new(big.Int).Mul(largeOut, small))
	if denominator.Sign() <= 0 {
		reserveIn = new(big.Int).Lsh(big.NewInt(1), rankFlatReserveBits)
	} else {
		reserveIn = numerator.Quo(numerator, denominator)
	}
	if reserveIn.Sign() <= 0 {
		return nil, nil, pool.ErrNoLiquidity
	}
	reserveOut = new(big.Int).Mul(reserveIn, smallOut)
	reserveOut.Quo(reserveOut, small)
	if reserveOut.Sign() <= 0 {
		return nil, nil, pool.ErrNoLiquidity
	}
	return reserveIn, reserveOut, nil
}

// SwapFee is the fee coarse ranking charges on top of the quote, in pips of
// the input: the notional fee, rounded up from 1e-8, and the current bin's
// additional fee for the direction. The spread itself is in the bid and ask,
// and the spread fee only splits it between the pool and the platform.
func SwapFee(p pool.State, tokenIn domain.Address) (uint32, error) {
	if p.Metric == nil || len(p.Metric.Words) == 0 {
		return 0, fmt.Errorf("%w: no MetricOmm state", pool.ErrInvalidState)
	}
	state, err := decodeMetricState(p.Metric)
	if err != nil {
		return 0, err
	}
	bin := state.Bins[int(state.CurBinIdx)-int(state.LowestBin)]
	fee := (state.NotionalFeeE8 + 99) / 100
	if tokenIn == p.Token0 {
		fee += uint32(bin.AddFeeSellE6)
	} else {
		fee += uint32(bin.AddFeeBuyE6)
	}
	if fee >= 1_000_000 {
		return 0, fmt.Errorf("%w: fee outside [0, 1000000)", pool.ErrInvalidState)
	}
	return fee, nil
}
