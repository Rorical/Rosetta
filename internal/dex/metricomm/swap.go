package metricomm

import (
	"fmt"
	"math/big"

	"github.com/holiman/uint256"
)

var (
	minInt128 = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 127))
	maxInt128 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 127), big.NewInt(1))
)

const (
	minInt24 = -1 << 23
	maxInt24 = 1<<23 - 1
)

// Swap runs one swap against state and returns the pool-side deltas in native
// token units (positive = into the pool) and the state after the swap. It
// reproduces MetricOmmPool.swap without its extension hooks, token transfers
// and callback, and leaves state untouched.
//
// amountSpecified is an exact input when positive and an exact output when
// negative. priceLimitX64 bounds the effective (fee-inclusive) price: a sell
// (zeroForOne) stops once the price falls to it, a buy once the price rises
// to it, so "no limit" is 0 for a sell and 2^128-1 for a buy. bidX64, askX64
// and referenceX64 are the price provider's quote, Q64.64 token1 per token0
// in whole tokens.
func Swap(
	state State,
	zeroForOne bool,
	amountSpecified, priceLimitX64, bidX64, askX64, referenceX64 *big.Int,
) (amount0Delta, amount1Delta *big.Int, next State, err error) {
	if err := state.validate(); err != nil {
		return nil, nil, State{}, err
	}
	if amountSpecified == nil || amountSpecified.Cmp(minInt128) < 0 || amountSpecified.Cmp(maxInt128) > 0 {
		return nil, nil, State{}, fmt.Errorf("%w: amount specified outside int128", ErrInvalidInput)
	}
	limit, err := uint128Word("price limit", priceLimitX64)
	if err != nil {
		return nil, nil, State{}, err
	}
	bid, err := uint128Word("bid", bidX64)
	if err != nil {
		return nil, nil, State{}, err
	}
	ask, err := uint128Word("ask", askX64)
	if err != nil {
		return nil, nil, State{}, err
	}
	reference, err := uint128Word("reference", referenceX64)
	if err != nil {
		return nil, nil, State{}, err
	}

	if state.PauseLevel != 0 {
		return nil, nil, State{}, ErrPoolPaused
	}
	e := &evm{}
	e.validateQuote(bid, ask, reference, state.BidAskRefAsymmetryX8)
	if e.err != nil {
		return nil, nil, State{}, e.err
	}
	if amountSpecified.Sign() == 0 {
		return nil, nil, State{}, ErrInvalidAmount
	}

	// The reference anchors the bin prices; bid and ask only enter here.
	var baseFee *word
	if zeroForOne {
		baseFee = e.checkedSub(e.mulDivUp(reference, oneX64, bid), oneX64)
	} else {
		baseFee = e.checkedSub(e.mulDivUp(ask, oneX64, reference), oneX64)
	}

	exactOut := amountSpecified.Sign() < 0
	magnitude, _ := uint256.FromBig(new(big.Int).Abs(amountSpecified))
	specifiedMultiplier := &state.Token0ScaleMultiplier
	if zeroForOne == exactOut {
		specifiedMultiplier = &state.Token1ScaleMultiplier
	}
	amountScaled := mul(specifiedMultiplier, magnitude)

	next = state.Clone()
	amountIn, amountOut, platformFees := e.swapAcrossBins(&next, zeroForOne, exactOut, amountScaled, reference, baseFee, limit)

	// Bin totals: the input enters the bins net of the platform fees.
	if zeroForOne {
		next.TotalToken0Scaled = *e.toUint128(sub(add(&next.TotalToken0Scaled, amountIn), platformFees))
		next.TotalToken1Scaled = *truncate128(sub(&next.TotalToken1Scaled, amountOut))
	} else {
		next.TotalToken1Scaled = *e.toUint128(sub(add(&next.TotalToken1Scaled, amountIn), platformFees))
		next.TotalToken0Scaled = *truncate128(sub(&next.TotalToken0Scaled, amountOut))
	}
	if e.err != nil {
		return nil, nil, State{}, e.err
	}

	inMultiplier, outMultiplier := &state.Token0ScaleMultiplier, &state.Token1ScaleMultiplier
	if !zeroForOne {
		inMultiplier, outMultiplier = outMultiplier, inMultiplier
	}
	if inMultiplier.IsZero() || outMultiplier.IsZero() {
		return nil, nil, State{}, ErrDivisionByZero
	}
	// deltasScaledToExternal rounds toward positive infinity: the input up,
	// the (negative) output toward zero.
	inDelta := e.ceilDiv(amountIn, inMultiplier).ToBig()
	outDelta := new(big.Int).Neg(new(word).Div(amountOut, outMultiplier).ToBig())
	if inDelta.Sign() == 0 || outDelta.Sign() == 0 {
		return nil, nil, State{}, ErrFinalAmountIsZero
	}
	if inDelta.Cmp(maxInt128) > 0 || outDelta.Cmp(minInt128) < 0 {
		return nil, nil, State{}, fmt.Errorf("%w: delta outside int128", ErrArithmeticOverflow)
	}
	if zeroForOne {
		return inDelta, outDelta, next, nil
	}
	return outDelta, inDelta, next, nil
}

func uint128Word(name string, value *big.Int) (*word, error) {
	if value == nil || value.Sign() < 0 || value.BitLen() > 128 {
		return nil, fmt.Errorf("%w: %s outside uint128", ErrInvalidInput, name)
	}
	converted, _ := uint256.FromBig(value)
	return converted, nil
}

// truncate128 is Solidity's unchecked uint128(x).
func truncate128(value *word) *word {
	return new(word).And(value, maxUint128)
}

// validateQuote is _validateBidAskReferencePriceX64: 0 < bid < ask, the
// reference within [bid, ask], and, unless disabled, 1/M <= r1/r2 <= M with
// r1 = ref/bid - 1, r2 = ask/ref - 1 and M = 1 + asymmetryX8/2^8.
func (e *evm) validateQuote(bid, ask, reference *word, asymmetryX8 uint16) {
	if !bid.Lt(ask) {
		e.revert(ErrBidGreaterThanAsk)
		return
	}
	if bid.IsZero() {
		e.revert(ErrBidIsZero)
		return
	}
	if reference.Lt(bid) || reference.Gt(ask) {
		e.revert(ErrReferencePriceOutOfRange)
		return
	}
	if asymmetryX8 == 0xffff {
		return
	}
	if reference.Eq(bid) || reference.Eq(ask) {
		e.revert(ErrBidAskRefAsymmetryOutOfBounds)
		return
	}

	boundX64 := add(oneX64, shl(uint256.NewInt(uint64(asymmetryX8)), 56))
	numerator := e.checkedMul(sub(reference, bid), reference)
	denominator := e.checkedMul(bid, sub(ask, reference))
	if numerator.Lt(e.mulDivUp(denominator, oneX64, boundX64)) || denominator.Lt(e.mulDivUp(numerator, oneX64, boundX64)) {
		e.revert(ErrBidAskRefAsymmetryOutOfBounds)
	}
}

// distanceToPrice is distanceE6ToPriceX64: reference * (1e6 + distance) / 1e6,
// rounded down. A distance below -1e6 wraps, as in the unchecked Solidity.
func (e *evm) distanceToPrice(distanceE6 int64, reference *word) *word {
	factor := new(word)
	if distanceE6 >= 0 {
		factor.AddUint64(e6, uint64(distanceE6))
	} else {
		factor.SubUint64(e6, uint64(-distanceE6))
	}
	return e.mulDiv(reference, factor, e6)
}

// effectivePrice is FeeMath.effectivePriceX64: sells divide the price by
// 1 + fee (rounded down), buys multiply by it (rounded up).
func (e *evm) effectivePrice(price, totalFee *word, zeroForOne bool) *word {
	onePlusFee := e.checkedAdd(oneX64, totalFee)
	if zeroForOne {
		return e.mulDiv(price, oneX64, onePlusFee)
	}
	return e.mulDivUp(price, onePlusFee, oneX64)
}

// feesOfBin derives the in-bin fee fractions from the bin's total fee
// (FeeMath.totalFeeOfGrossX64 and FeeMath.platformPartOfTotalFeeX64).
func (e *evm) feesOfBin(totalFee, notionalFee *word, spreadFeeE6 uint32) binFees {
	if totalFee.IsZero() {
		return binFees{totalOfGross: zero, platformShare: zero}
	}
	spreadPart := new(word).Div(mul(sub(totalFee, notionalFee), uint256.NewInt(uint64(spreadFeeE6))), e6)
	return binFees{
		totalOfGross:  e.mulDiv(totalFee, oneX64, e.checkedAdd(oneX64, totalFee)),
		platformShare: e.mulDiv(add(notionalFee, spreadPart), oneX64, totalFee),
	}
}

// swapAcrossBins is _swapAcrossBins: it walks the bins from the current one,
// mutating next, until the amount is filled, the price limit is reached or
// the grid ends. It returns the scaled input, output and platform fees.
func (e *evm) swapAcrossBins(
	next *State,
	zeroForOne, exactOut bool,
	amountScaled, reference, baseFee, priceLimit *word,
) (amountIn, amountOut, platformFees *word) {
	totalOut := &next.TotalToken0Scaled
	if zeroForOne {
		totalOut = &next.TotalToken1Scaled
	}
	if exactOut && amountScaled.Gt(totalOut) {
		amountScaled = totalOut.Clone()
	}
	if amountScaled.Gt(maxUint128) {
		e.revert(ErrAmountScaledExceedsMax)
		return zero, zero, zero
	}
	swap := &progress{remaining: amountScaled, calculated: zero}

	binIdx := int(next.CurBinIdx)
	pos := next.CurPosInBin.Clone()
	distE6 := int64(next.CurBinDistFromProvidedPriceE6)
	bin := next.Bins[binIdx-int(next.LowestBin)]
	lowerRef := e.distanceToPrice(distE6, reference)
	upperRef := e.distanceToPrice(distE6+int64(bin.LengthE6), reference)

	notionalFee := new(word).Div(mul(uint256.NewInt(uint64(next.NotionalFeeE8)), oneX64), e8)
	// Exact input only: output left across all bins (exact output is capped above).
	availableOut := zero
	if !exactOut {
		availableOut = totalOut.Clone()
	}
	platformFees = zero

	for !swap.remaining.IsZero() && e.err == nil {
		addFeeE6 := bin.AddFeeBuyE6
		if zeroForOne {
			addFeeE6 = bin.AddFeeSellE6
		}
		binTotalFee := add(add(baseFee, new(word).Div(mul(uint256.NewInt(uint64(addFeeE6)), oneX64), e6)), notionalFee)
		lowerEff := e.effectivePrice(lowerRef, binTotalFee, zeroForOne)
		upperEff := e.effectivePrice(upperRef, binTotalFee, zeroForOne)

		// A bin with nothing left to give in this direction is skipped,
		// unless the price limit or the pool's total output ends the swap.
		nonEmpty := true
		if zeroForOne {
			if bin.Token1BalanceScaled.IsZero() || pos.IsZero() {
				if (exactOut || !priceLimit.IsZero()) && !priceLimit.Lt(lowerEff) {
					break
				}
				if !exactOut && availableOut.IsZero() {
					break
				}
				nonEmpty = false
			}
		} else if bin.Token0BalanceScaled.IsZero() || !pos.Lt(maxPosBin) {
			if (exactOut || !priceLimit.IsZero()) && !priceLimit.Gt(upperEff) {
				break
			}
			if !exactOut && availableOut.IsZero() {
				break
			}
			nonEmpty = false
		}

		if nonEmpty {
			fees := e.feesOfBin(binTotalFee, notionalFee, next.SpreadFeeE6)
			var step binStep
			switch {
			case zeroForOne && exactOut:
				step = e.buyToken1ExactOut(&bin, pos, swap, fees, lowerEff, upperEff, priceLimit)
			case zeroForOne:
				step = e.buyToken1ExactIn(&bin, pos, swap, fees, lowerEff, upperEff, priceLimit)
			case exactOut:
				step = e.buyToken0ExactOut(&bin, pos, swap, fees, lowerEff, upperEff, priceLimit)
			default:
				step = e.buyToken0ExactIn(&bin, pos, swap, fees, lowerEff, upperEff, priceLimit)
			}
			pos = step.finalPos
			if !exactOut {
				availableOut = sub(availableOut, step.out)
			}
			if step.touched {
				platformFees = add(platformFees, step.platformFee)
				next.Bins[binIdx-int(next.LowestBin)] = bin
			}
		}

		exhausted := !pos.Lt(maxPosBin)
		if zeroForOne {
			exhausted = pos.IsZero()
		}
		if !exhausted && nonEmpty {
			break
		}
		if zeroForOne {
			if binIdx <= int(next.LowestBin) {
				break
			}
			binIdx--
			bin = next.Bins[binIdx-int(next.LowestBin)]
			pos = maxPosBin
			distE6 -= int64(bin.LengthE6)
			upperRef = lowerRef
			lowerRef = e.distanceToPrice(distE6, reference)
		} else {
			if binIdx >= int(next.HighestBin) {
				break
			}
			binIdx++
			pos = zero
			distE6 = min(max(distE6+int64(bin.LengthE6), minInt24), maxInt24)
			lowerRef = upperRef
			bin = next.Bins[binIdx-int(next.LowestBin)]
			upperRef = e.distanceToPrice(distE6+int64(bin.LengthE6), reference)
		}
	}

	// _finalizeSwap narrows with checked casts; the bin index stays inside
	// the int16 grid by construction.
	if pos.Gt(maxPosBin) || distE6 < minInt24 || distE6 > maxInt24 {
		e.revert(ErrArithmeticOverflow)
	}
	next.CurBinIdx = int16(binIdx)
	next.CurPosInBin = *pos
	next.CurBinDistFromProvidedPriceE6 = int32(distE6)

	if exactOut {
		return swap.calculated, sub(amountScaled, swap.remaining), platformFees
	}
	return sub(amountScaled, swap.remaining), swap.calculated, platformFees
}
