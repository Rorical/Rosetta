package math

import (
	"fmt"
	"math/big"
)

// CoverageTicks is the price distance a loaded tick window must cover on each
// side of the current tick: ceil(-ln(0.8)/ln(1.0001)), a 20% fall, which also
// covers a 22% rise. A single-pool move that large needs a dislocation of the
// same size, so quotes beyond it are treated as the pool's capacity, not as
// missing tick data.
const CoverageTicks = 2232

// QuoteV3ExactIn prices an exact-input swap the way the executor settles it.
// SwapExecutor and V4SwapExecutor revert unless the pool takes the whole
// input, so a swap that stops at the price bound with input left over is
// ErrCapacity, not a partial fill. Window errors are those of SwapV3ExactIn.
func QuoteV3ExactIn(state V3State, zeroForOne bool, amountIn *big.Int) (*big.Int, error) {
	result, err := SwapV3ExactIn(state, zeroForOne, amountIn)
	if err != nil {
		return nil, err
	}
	if result.HitLimit {
		return nil, fmt.Errorf("%w: %s of %s input fills before the price bound", ErrCapacity, result.AmountInUsed, amountIn)
	}
	return result.AmountOut, nil
}

// checkWindow classifies a final tick outside the loaded window. Every step
// between the start and final tick used only loaded words, so the exit side is
// fixed by the swap direction. If that side already covered CoverageTicks, more
// ticks would only price a move beyond the coverage policy: ErrCapacity. A
// narrower side is missing data and returns ErrOutOfWindow so it is reloaded.
func checkWindow(state V3State, zeroForOne bool, tick int32) error {
	if state.WindowMin == 0 && state.WindowMax == 0 || tick >= state.WindowMin && tick <= state.WindowMax {
		return nil
	}
	if state.Tick < state.WindowMin || state.Tick > state.WindowMax {
		return fmt.Errorf("%w: start tick %d outside [%d, %d]", ErrOutOfWindow, state.Tick, state.WindowMin, state.WindowMax)
	}
	covered := int64(state.WindowMax) - int64(state.Tick)
	if zeroForOne {
		covered = int64(state.Tick) - int64(state.WindowMin)
	}
	if covered >= CoverageTicks {
		return fmt.Errorf("%w: final tick %d leaves [%d, %d] after %d covered ticks",
			ErrCapacity, tick, state.WindowMin, state.WindowMax, covered)
	}
	return fmt.Errorf("%w: final tick %d outside [%d, %d]", ErrOutOfWindow, tick, state.WindowMin, state.WindowMax)
}
