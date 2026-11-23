package metricomm

// This file ports libraries/SwapMath.sol. Function order and rounding follow
// the Solidity; prices named lower/upper are effective (fee-inclusive) Q64.64
// prices of the bin edges.

// progress is SwapMath.SwapState.
type progress struct {
	remaining  *word // amountSpecifiedRemainingScaled
	calculated *word // amountCalculatedScaled
}

// binFees are the per-bin fee fractions passed to every in-bin step.
type binFees struct {
	totalOfGross  *word // FeeMath.totalFeeOfGrossX64
	platformShare *word // FeeMath.platformPartOfTotalFeeX64
}

// binStep is the outcome of one in-bin step.
type binStep struct {
	finalPos    *word
	out         *word // output taken from the bin
	platformFee *word // withheld from the input, never enters the bin
	// touched reports a non-zero bin delta: only then does the pool store the
	// bin and account the fees.
	touched bool
}

func untouched(pos *word) binStep {
	return binStep{finalPos: pos, out: zero, platformFee: zero}
}

func (e *evm) invertPrice(priceX64 *word) *word {
	return e.ceilDiv(oneX128, priceX64)
}

// priceAtBinPosition is calculatePriceAtBinPosition: linear interpolation
// between the bin edges.
func (e *evm) priceAtBinPosition(lower, upper, position *word, roundUp bool) *word {
	maxSubPos := e.checkedSub(maxPosBin, position)
	weighted := add(mul(lower, maxSubPos), mul(upper, position))
	if roundUp {
		return e.ceilDiv(weighted, maxPosBin)
	}
	return e.div(weighted, maxPosBin)
}

// binPositionAtPrice is calculateBinPositionAtPrice, the inverse interpolation.
func (e *evm) binPositionAtPrice(lower, upper, price *word, roundUp bool) *word {
	numerator := mul(sub(price, lower), maxPosBin)
	width := sub(upper, lower)
	if roundUp {
		return e.ceilDiv(numerator, width)
	}
	return e.div(numerator, width)
}

// positionAfterSellingAmount0 is calculateBinPositionAfterSellingAmount0.
func (e *evm) positionAfterSellingAmount0(pos, traded0, available0 *word) *word {
	return add(pos, e.div(mul(sub(maxPosBin, pos), traded0), available0))
}

// positionAfterSellingAmount1 is calculateBinPositionAfterSellingAmount1.
func (e *evm) positionAfterSellingAmount1(pos, traded1, available1 *word) *word {
	return e.ceilDiv(mul(pos, sub(available1, traded1)), available1)
}

// outputToken1FromPosition is calculateOutputToken1FromBinPosition.
func (e *evm) outputToken1FromPosition(available1, pos, finalPos *word) *word {
	return e.div(mul(available1, sub(pos, finalPos)), pos)
}

// outputToken0FromPosition is calculateOutputToken0FromBinPosition.
func (e *evm) outputToken0FromPosition(available0, pos, finalPos *word) *word {
	return e.div(mul(available0, sub(finalPos, pos)), sub(maxPosBin, pos))
}

// arithmeticMean is calculateArithmeticMean, rounded up.
func arithmeticMean(a, b *word) *word {
	sum := add(add(a, b), one)
	return sum.Rsh(sum, 1)
}

// requiredToken1 is calculateRequiredToken1: token1 input for a token0 output
// priced at the mean of the start and final prices.
func (e *evm) requiredToken1(token0Out, startPrice, finalPrice *word) *word {
	startCost := e.mulDivUp(token0Out, startPrice, oneX64)
	finalCost := e.mulDivUp(token0Out, finalPrice, oneX64)
	return arithmeticMean(startCost, finalCost)
}

// requiredToken0 is calculateRequiredToken0.
func (e *evm) requiredToken0(token1Out, startPrice, finalPrice *word) *word {
	startCost := e.mulDivUp(token1Out, oneX64, startPrice)
	finalCost := e.mulDivUp(token1Out, oneX64, finalPrice)
	return arithmeticMean(startCost, finalCost)
}

// analyticalTargetPosForBuyToken0 is computeAnalyticalTargetPosForBuyToken0:
// the closed-form position reached by spending inputAmount of token1. The
// Solidity runs checked, so every operation here reverts on overflow.
func (e *evm) analyticalTargetPosForBuyToken0(pos, maxFinalPos, inputAmount, token0Balance, lower, upper *word) *word {
	maxSubPos := e.checkedSub(maxPosBin, pos)
	deltaPrice := e.checkedSub(upper, lower)
	priceAtPos := e.checkedAdd(lower, e.div(e.checkedMul(deltaPrice, pos), maxPosBin))

	deltaPos := new(word)
	if !token0Balance.IsZero() && !priceAtPos.IsZero() && !maxSubPos.IsZero() {
		aX128 := e.mulDiv(token0Balance, shl(priceAtPos, 64), maxSubPos)
		if !aX128.IsZero() {
			qX128 := e.mulDiv(shl(inputAmount, 128), oneX128, aX128)
			rQX128 := e.mulDiv(deltaPrice, qX128, e.checkedMul(e.checkedMul(two, maxPosBin), priceAtPos))
			sqrtArgX128 := e.checkedAdd(oneX128, e.checkedMul(four, rQX128))
			sqrtValX64 := new(word).Sqrt(sqrtArgX128)
			deltaPos = e.div(e.checkedMul(two, qX128), e.checkedAdd(oneX128, shl(sqrtValX64, 64)))
		}
	}

	if deltaPos.IsZero() && !inputAmount.IsZero() {
		deltaPos = one
	}
	if deltaPos.Gt(maxSubPos) {
		deltaPos = maxSubPos
	}
	targetPos := e.checkedAdd(pos, deltaPos)
	if targetPos.Gt(maxFinalPos) {
		targetPos = maxFinalPos
	}
	if !targetPos.Gt(pos) {
		targetPos = pos
		if pos.Lt(maxFinalPos) {
			targetPos = e.checkedAdd(pos, one)
		}
	}
	return targetPos
}

// analyticalTargetPosForSellToken0 is computeAnalyticalTargetPosForSellToken0:
// the buy solution on inverted prices and mirrored positions.
func (e *evm) analyticalTargetPosForSellToken0(pos, minFinalPos, inputAmount, token1Balance, lower, upper *word) *word {
	mirrored := e.analyticalTargetPosForBuyToken0(
		e.checkedSub(maxPosBin, pos),
		e.checkedSub(maxPosBin, minFinalPos),
		inputAmount,
		token1Balance,
		e.invertPrice(upper),
		e.invertPrice(lower),
	)
	return e.checkedSub(maxPosBin, mirrored)
}

// incomingBinBalance is _toUint104IncomingBinBalance.
func (e *evm) incomingBinBalance(value *word) *word {
	if value.Gt(maxPosBin) {
		e.revert(ErrBinBalanceOverflow)
	}
	return value
}

// platformFeeOf splits the carved fee of an input and returns the platform's
// part; the rest stays in the bin with the input.
func (e *evm) platformFeeOf(amountIn *word, fees binFees) *word {
	totalFee := e.div(mul(amountIn, fees.totalOfGross), oneX64)
	return e.div(mul(totalFee, fees.platformShare), oneX64)
}

// settleBuyToken0 books a step that takes token0 out of the bin for token1.
func (e *evm) settleBuyToken0(bin *Bin, amountOut, amountIn *word, fees binFees) (platformFee *word, touched bool) {
	platformFee = e.platformFeeOf(amountIn, fees)
	binCredit := sub(amountIn, platformFee)
	bin.Token0BalanceScaled = *sub(&bin.Token0BalanceScaled, e.toUint104(amountOut))
	bin.Token1BalanceScaled = *e.incomingBinBalance(add(&bin.Token1BalanceScaled, binCredit))
	return platformFee, !amountOut.IsZero() || !binCredit.IsZero()
}

// settleBuyToken1 books a step that takes token1 out of the bin for token0.
func (e *evm) settleBuyToken1(bin *Bin, amountOut, amountIn *word, fees binFees) (platformFee *word, touched bool) {
	platformFee = e.platformFeeOf(amountIn, fees)
	binCredit := sub(amountIn, platformFee)
	bin.Token1BalanceScaled = *sub(&bin.Token1BalanceScaled, e.toUint104(amountOut))
	bin.Token0BalanceScaled = *e.incomingBinBalance(add(&bin.Token0BalanceScaled, binCredit))
	return platformFee, !amountOut.IsZero() || !binCredit.IsZero()
}

// buyToken0ExactOut is buyToken0InBinSpecifiedOut (token1 in, exact token0 out).
func (e *evm) buyToken0ExactOut(bin *Bin, pos *word, swap *progress, fees binFees, lower, upper, priceLimit *word) binStep {
	available0 := &bin.Token0BalanceScaled
	var finalPos, amountOut *word
	if swap.remaining.Lt(available0) {
		finalPos = e.positionAfterSellingAmount0(pos, swap.remaining, available0)
		amountOut = swap.remaining
	} else {
		finalPos = maxPosBin
		amountOut = available0.Clone()
	}

	if priceLimit.Lt(upper) {
		if !priceLimit.Gt(lower) {
			return untouched(pos)
		}
		posAtLimit := e.binPositionAtPrice(lower, upper, priceLimit, false)
		if !posAtLimit.Gt(pos) {
			return untouched(pos)
		}
		if posAtLimit.Lt(finalPos) {
			finalPos = posAtLimit
			amountOut = minWord(e.outputToken0FromPosition(available0, pos, finalPos), amountOut)
		}
	}

	if amountOut.IsZero() {
		return untouched(pos)
	}

	startPrice := e.priceAtBinPosition(lower, upper, pos, true)
	finalPrice := e.priceAtBinPosition(lower, upper, finalPos, true)
	amountIn := e.requiredToken1(amountOut, startPrice, finalPrice)

	platformFee, touched := e.settleBuyToken0(bin, amountOut, amountIn, fees)
	swap.remaining = sub(swap.remaining, amountOut)
	swap.calculated = add(swap.calculated, amountIn)
	return binStep{finalPos: finalPos, out: amountOut, platformFee: platformFee, touched: touched}
}

// buyToken1ExactOut is buyToken1InBinSpecifiedOut (token0 in, exact token1 out).
func (e *evm) buyToken1ExactOut(bin *Bin, pos *word, swap *progress, fees binFees, lower, upper, priceLimit *word) binStep {
	available1 := &bin.Token1BalanceScaled
	var finalPos, amountOut *word
	if swap.remaining.Lt(available1) {
		finalPos = e.positionAfterSellingAmount1(pos, swap.remaining, available1)
		amountOut = swap.remaining
	} else {
		finalPos = zero
		amountOut = available1.Clone()
	}

	if lower.Lt(priceLimit) {
		if !priceLimit.Lt(upper) {
			return untouched(pos)
		}
		posAtLimit := e.binPositionAtPrice(lower, upper, priceLimit, true)
		if !posAtLimit.Lt(pos) {
			return untouched(pos)
		}
		if finalPos.Lt(posAtLimit) {
			finalPos = posAtLimit
			amountOut = minWord(e.outputToken1FromPosition(available1, pos, finalPos), amountOut)
		}
	}

	if amountOut.IsZero() {
		return untouched(pos)
	}

	startPrice := e.priceAtBinPosition(lower, upper, pos, false)
	finalPrice := e.priceAtBinPosition(lower, upper, finalPos, false)
	amountIn := e.requiredToken0(amountOut, startPrice, finalPrice)

	platformFee, touched := e.settleBuyToken1(bin, amountOut, amountIn, fees)
	swap.remaining = sub(swap.remaining, amountOut)
	swap.calculated = add(swap.calculated, amountIn)
	return binStep{finalPos: finalPos, out: amountOut, platformFee: platformFee, touched: touched}
}

// buyToken0ExactIn is buyToken0InBinSpecifiedIn (exact token1 in, token0 out).
// It tries the whole reachable range first, then the closed-form target with
// at most one proportional rescale; leftover input inside the bin is charged
// in full and an overshoot scales the output down.
func (e *evm) buyToken0ExactIn(bin *Bin, pos *word, swap *progress, fees binFees, lower, upper, priceLimit *word) binStep {
	available0 := bin.Token0BalanceScaled.Clone()
	remaining := swap.remaining
	startPrice := e.priceAtBinPosition(lower, upper, pos, true)
	if shl(remaining, 64).Lt(startPrice) {
		return untouched(pos)
	}

	maxFinalPos := maxPosBin
	if upper.Gt(priceLimit) {
		if !priceLimit.Gt(lower) {
			return untouched(pos)
		}
		maxFinalPos = e.binPositionAtPrice(lower, upper, priceLimit, false)
		if !maxFinalPos.Gt(pos) {
			return untouched(pos)
		}
	}

	quote := func(target *word) (out, totalIn *word) {
		out = e.outputToken0FromPosition(available0, pos, target)
		finalPrice := e.priceAtBinPosition(lower, upper, target, true)
		return out, e.requiredToken1(out, startPrice, finalPrice)
	}

	targetPos := maxFinalPos
	amountOut, totalIn := quote(maxFinalPos)
	if totalIn.Gt(remaining) {
		targetPos = e.analyticalTargetPosForBuyToken0(pos, maxFinalPos, remaining, available0, lower, upper)
		amountOut, totalIn = quote(targetPos)

		if totalIn.Lt(remaining) && targetPos.Lt(maxFinalPos) {
			if totalIn.IsZero() {
				totalIn = one
			}
			scaledDelta := e.div(mul(sub(targetPos, pos), remaining), totalIn)
			if scaledDelta.IsZero() {
				totalIn = remaining
				targetPos = pos
				amountOut = zero
			} else {
				targetPos = minWord(add(pos, scaledDelta), maxFinalPos)
				amountOut, totalIn = quote(targetPos)
			}
		}

		if totalIn.Lt(remaining) && targetPos.Lt(maxFinalPos) {
			totalIn = remaining
		}

		if totalIn.Gt(remaining) {
			amountOut = e.div(mul(amountOut, remaining), totalIn)
			targetPos = e.positionAfterSellingAmount0(pos, amountOut, available0)
			totalIn = remaining
		}
	}

	if amountOut.IsZero() {
		targetPos = pos
	}

	platformFee, touched := e.settleBuyToken0(bin, amountOut, totalIn, fees)
	swap.remaining = sub(remaining, totalIn)
	swap.calculated = add(swap.calculated, amountOut)
	return binStep{finalPos: targetPos, out: amountOut, platformFee: platformFee, touched: touched}
}

// buyToken1ExactIn is buyToken1InBinSpecifiedIn (exact token0 in, token1 out),
// the mirror of buyToken0ExactIn moving down in price.
func (e *evm) buyToken1ExactIn(bin *Bin, pos *word, swap *progress, fees binFees, lower, upper, priceLimit *word) binStep {
	available1 := bin.Token1BalanceScaled.Clone()
	remaining := swap.remaining
	startPrice := e.priceAtBinPosition(lower, upper, pos, false)
	if remaining.Lt(e.ceilDiv(oneX64, startPrice)) {
		return untouched(pos)
	}

	minFinalPos := zero
	if priceLimit.Gt(lower) {
		minFinalPos = e.binPositionAtPrice(lower, upper, priceLimit, true)
		if !pos.Gt(minFinalPos) {
			return untouched(pos)
		}
	}

	quote := func(target *word) (out, totalIn *word) {
		out = e.outputToken1FromPosition(available1, pos, target)
		finalPrice := e.priceAtBinPosition(lower, upper, target, false)
		return out, e.requiredToken0(out, startPrice, finalPrice)
	}

	targetPos := minFinalPos
	amountOut, totalIn := quote(minFinalPos)
	if totalIn.Gt(remaining) {
		targetPos = e.analyticalTargetPosForSellToken0(pos, minFinalPos, remaining, available1, lower, upper)
		amountOut, totalIn = quote(targetPos)

		if totalIn.Lt(remaining) && targetPos.Gt(minFinalPos) {
			if totalIn.IsZero() {
				totalIn = one
			}
			scaledDelta := e.div(mul(sub(pos, targetPos), remaining), totalIn)
			if scaledDelta.IsZero() {
				totalIn = remaining
				targetPos = pos
				amountOut = zero
			} else {
				targetPos = zero
				if pos.Gt(scaledDelta) {
					targetPos = sub(pos, scaledDelta)
				}
				if targetPos.Lt(minFinalPos) {
					targetPos = minFinalPos
				}
				amountOut, totalIn = quote(targetPos)
			}
		}

		if totalIn.Lt(remaining) && targetPos.Gt(minFinalPos) {
			totalIn = remaining
		}

		if totalIn.Gt(remaining) {
			amountOut = e.div(mul(amountOut, remaining), totalIn)
			targetPos = e.positionAfterSellingAmount1(pos, amountOut, available1)
			totalIn = remaining
		}
	}

	if amountOut.IsZero() {
		targetPos = pos
	}

	platformFee, touched := e.settleBuyToken1(bin, amountOut, totalIn, fees)
	swap.remaining = sub(remaining, totalIn)
	swap.calculated = add(swap.calculated, amountOut)
	return binStep{finalPos: targetPos, out: amountOut, platformFee: platformFee, touched: touched}
}
