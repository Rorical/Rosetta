package arb

import (
	stdmath "math"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

type rankPool struct {
	state pool.State
	ok    bool
	legs  [2]rankLeg
	errs  [2]error
}

type rankLeg struct {
	model    math.ClosedFormLeg
	curve    math.CycleCoefficients
	curveErr error
	// rateUpper and rateLower enclose the marginal rate after fee; rateLower
	// is zero where rateUpper is infinite.
	rateUpper, rateLower float64
	interval             modelInterval
	// A non-CP model must not participate in CP proofs of unprofitability.
	approximate bool
}

func prepareRankPool(p pool.State, ok bool, opts RankedOptions) rankPool {
	cached := rankPool{state: p, ok: ok}
	if !ok {
		return cached
	}
	var in, out, reverseIn, reverseOut *big.Int
	var liquid bool
	approximate := false
	var modelErr error
	if opts.Model == nil {
		in, out, liquid = pool.SpotReserves(p)
	} else {
		model, err := opts.Model(p)
		in, out, modelErr = model.Reserve0, model.Reserve1, err
		reverseIn, reverseOut = model.ReverseIn, model.ReverseOut
		liquid, approximate = err == nil, !model.ConstantProduct
	}
	for direction, token := range [2]domain.Address{p.Token0, p.Token1} {
		if direction == 1 && reverseIn != nil {
			in, out = reverseIn, reverseOut
		}
		fee, err := opts.SwapFee(p, token)
		if err == nil {
			err = modelErr
		}
		if err == nil && !liquid {
			err = pool.ErrNoLiquidity
		}
		leg := math.ClosedFormLeg{InReserve: in, OutReserve: out, FeePips: fee}
		cached.legs[direction] = rankLeg{model: leg, rateUpper: stdmath.Inf(1), approximate: approximate, interval: newModelInterval(leg)}
		if err == nil && fee < 1_000_000 && !approximate && cached.legs[direction].interval.valid {
			cached.legs[direction].rateUpper = cached.legs[direction].interval.rateHigh
			cached.legs[direction].rateLower = cached.legs[direction].interval.rateLow
		}
		cached.legs[direction].curveErr = cached.legs[direction].curve.SetLeg(leg)
		cached.errs[direction] = err
		in, out = out, in
	}
	return cached
}

func captureFunding(opts RankedOptions, token domain.Address) rankFunding {
	bounds, ok := opts.Bounds(token)
	if ok {
		if bounds.Min != nil {
			bounds.Min = new(big.Int).Set(bounds.Min)
		}
		if bounds.Max != nil {
			bounds.Max = new(big.Int).Set(bounds.Max)
		}
	}
	funding := rankFunding{bounds: bounds, ok: ok, valid: validBounds(bounds)}
	if funding.valid {
		funding.low, _ = integerInterval(bounds.Min)
		_, funding.high = integerInterval(bounds.Max)
	}
	return funding
}

func validBounds(b InputBounds) bool {
	return b.Min != nil && b.Max != nil && b.Min.Sign() > 0 && b.Min.Cmp(b.Max) <= 0 && b.Max.BitLen() <= 256
}

// Rat.Float64 rounds to nearest. Moving one representable value upward encloses
// the exact positive rate, including underflow; overflow remains +Inf. Each
// subsequent multiplication is rounded upward too. An upper bound <=1 proves
// rejection; uncertain/break-even bounds still use the integer product test.
func marginalRateUpper(leg math.ClosedFormLeg) float64 {
	numerator := new(big.Int).Mul(leg.OutReserve, big.NewInt(int64(1_000_000-leg.FeePips)))
	denominator := new(big.Int).Mul(leg.InReserve, big.NewInt(1_000_000))
	rate, exact := new(big.Rat).SetFrac(numerator, denominator).Float64()
	if !exact {
		rate = stdmath.Nextafter(rate, stdmath.Inf(1))
	}
	return rate
}

// marginProfitable is CycleCanProfit for constant-product legs whose rate
// products are enclosed by upper and lower, each rounded outward at every leg.
// Outside that rounding band the floats decide; the integer product runs only
// inside it. Run for every route with an upper product above one, it cost
// about 13% of search CPU.
func marginProfitable(upper, lower float64, legs []math.ClosedFormLeg) bool {
	if upper <= 1 {
		return false
	}
	if lower > 1 {
		return true
	}
	possible, err := math.CycleCanProfit(legs)
	return err == nil && possible
}
