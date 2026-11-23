package math

import (
	"fmt"
	"math/big"
	"math/rand"
	"testing"
)

// The allocating helper composition remains independent of scratch arithmetic,
// including amount0's two rounding stages and the uint256 overflow fallback.
func referenceSwapStep(sqrtCurrent, sqrtTarget, liquidity, amountRemaining *big.Int, feePips uint32) (swapStep, error) {
	var zero swapStep

	feeComplement := big.NewInt(int64(feePipsDenominator - feePips))
	remainingLessFee, err := MulDiv(amountRemaining, feeComplement, feePipsDenominatorBig)
	if err != nil {
		return zero, fmt.Errorf("compute swap step: %w", err)
	}

	zeroForOne := sqrtCurrent.Cmp(sqrtTarget) >= 0

	var amountInToTarget *big.Int
	if zeroForOne {
		amountInToTarget, err = amount0Delta(sqrtTarget, sqrtCurrent, liquidity, true)
	} else {
		amountInToTarget, err = amount1Delta(sqrtCurrent, sqrtTarget, liquidity, true)
	}
	if err != nil {
		return zero, fmt.Errorf("compute swap step: %w", err)
	}

	reachedTarget := remainingLessFee.Cmp(amountInToTarget) >= 0
	var next *big.Int
	if reachedTarget {
		next = new(big.Int).Set(sqrtTarget)
	} else {
		if zeroForOne {
			next, err = NextSqrtPriceFromAmount0In(sqrtCurrent, liquidity, remainingLessFee)
		} else {
			next, err = NextSqrtPriceFromAmount1In(sqrtCurrent, liquidity, remainingLessFee)
		}
		if err != nil {
			return zero, fmt.Errorf("compute swap step: %w", err)
		}
		reachedTarget = next.Cmp(sqrtTarget) == 0
	}

	var amountIn, amountOut *big.Int
	if zeroForOne {
		amountIn, err = amount0Delta(next, sqrtCurrent, liquidity, true)
		if err == nil {
			amountOut, err = amount1Delta(next, sqrtCurrent, liquidity, false)
		}
	} else {
		amountIn, err = amount1Delta(sqrtCurrent, next, liquidity, true)
		if err == nil {
			amountOut, err = amount0Delta(sqrtCurrent, next, liquidity, false)
		}
	}
	if err != nil {
		return zero, fmt.Errorf("compute swap step: %w", err)
	}

	var fee *big.Int
	if reachedTarget {
		fee, err = mulDivRoundingUp(amountIn, big.NewInt(int64(feePips)), feeComplement)
		if err != nil {
			return zero, fmt.Errorf("compute swap step: %w", err)
		}
	} else {
		// The partial step consumes the whole remaining input; the fee is
		// whatever the delta calculation left over.
		fee = new(big.Int).Sub(amountRemaining, amountIn)
	}

	return swapStep{
		sqrtPriceNext: next,
		amountIn:      amountIn,
		amountOut:     amountOut,
		fee:           fee,
	}, nil
}

func TestSwapWorkspaceMatchesAllocatingReference(t *testing.T) {
	random := rand.New(rand.NewSource(95128))
	work := new(swapWorkspace)
	for i := range 1200 {
		tick := int32(random.Intn(1700000) - 850000)
		targetTick := max(MinTick, min(MaxTick, tick+int32(random.Intn(20001)-10000)))
		current := sqrtAt(t, tick)
		target := sqrtAt(t, targetTick)
		liquidity := new(big.Int).Lsh(big.NewInt(int64(random.Intn(1000000))), uint(random.Intn(140)))
		if i%31 == 0 {
			liquidity.SetInt64(0)
		}
		amount := new(big.Int).Lsh(big.NewInt(int64(1+random.Intn(1000000))), uint(random.Intn(236)))
		fee := []uint32{0, 1, 100, 3000, 999999}[i%5]
		want, err := referenceSwapStep(current, target, liquidity, amount, fee)
		got, actualErr := work.computeStep(current, target, liquidity, amount, fee)
		if (err == nil) != (actualErr == nil) {
			t.Fatalf("sample %d errors: reference=%v scratch=%v", i, err, actualErr)
		}
		if err != nil {
			continue
		}
		for field, pair := range [][2]*big.Int{{got.sqrtPriceNext, want.sqrtPriceNext}, {got.amountIn, want.amountIn}, {got.amountOut, want.amountOut}, {got.fee, want.fee}} {
			if pair[0].Cmp(pair[1]) != 0 {
				t.Fatalf("sample %d field %d tick %d->%d liquidity=%s input=%s fee=%d: got=%s want=%s", i, field, tick, targetTick, liquidity, amount, fee, pair[0], pair[1])
			}
		}
	}
}

func TestSwapWorkspaceDoesNotEscapeThroughResults(t *testing.T) {
	state := crossingState()
	input := big.NewInt(2_000_000_000_000_000)
	first, err := SwapV3ExactIn(state, true, input)
	if err != nil {
		t.Fatal(err)
	}
	saved := []string{first.AmountOut.String(), first.AmountInUsed.String(), first.SqrtPriceX96.String(), first.Liquidity.String()}
	for range 100 {
		again, err := SwapV3ExactIn(state, true, input)
		if err != nil {
			t.Fatal(err)
		}
		again.AmountOut.SetInt64(0)
		again.AmountInUsed.SetInt64(0)
		again.SqrtPriceX96.SetInt64(0)
		again.Liquidity.SetInt64(0)
	}
	for i, value := range []*big.Int{first.AmountOut, first.AmountInUsed, first.SqrtPriceX96, first.Liquidity} {
		if value.String() != saved[i] {
			t.Fatal("result aliases reused workspace", i)
		}
	}
}
