package uniswapv3

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// EventKind identifies the V3 pool event that produced an Event.
type EventKind uint8

const (
	EventInitialize EventKind = iota + 1
	EventMint
	EventBurn
	EventSwap
	EventCollect
)

// Event is the typed subset of a V3 log needed to advance pool state. Only the
// fields named by Kind are read.
type Event struct {
	Kind         EventKind
	TickLower    int32
	TickUpper    int32
	Amount       *big.Int
	SqrtPriceX96 *big.Int
	Tick         int32
	Liquidity    *big.Int
}

// Apply advances state and deep by one event. Both must be non-nil. Maps are
// created on first write and big.Int fields written by the reducer are never
// left nil.
func Apply(state *pool.State, deep *pool.DeepState, ev Event) error {
	if state == nil || deep == nil {
		return fmt.Errorf("%w: nil state or deep state", ErrInvalidEvent)
	}
	switch ev.Kind {
	case EventInitialize:
		return applyInitialize(state, deep, ev)
	case EventMint:
		return applyMint(state, deep, ev)
	case EventBurn:
		return applyBurn(state, deep, ev)
	case EventSwap:
		return applySwap(state, ev)
	case EventCollect:
		return nil
	default:
		return fmt.Errorf("%w: kind %d", ErrInvalidEvent, ev.Kind)
	}
}

func applyInitialize(state *pool.State, deep *pool.DeepState, ev Event) error {
	if ev.SqrtPriceX96 == nil || ev.SqrtPriceX96.Sign() <= 0 {
		return fmt.Errorf("%w: initialize sqrt price must be positive", ErrInvalidEvent)
	}
	state.SqrtPriceX96 = new(big.Int).Set(ev.SqrtPriceX96)
	state.Tick = ev.Tick
	state.Liquidity = new(big.Int)
	deep.Bitmap = make(map[int16]*big.Int)
	deep.Ticks = make(map[int32]pool.TickInfo)
	// The old bitmap, and with it the tick range it covered, is gone.
	deep.WindowMin = 0
	deep.WindowMax = 0
	return nil
}

func applyMint(state *pool.State, deep *pool.DeepState, ev Event) error {
	if ev.Amount == nil || ev.Amount.Sign() < 0 {
		return fmt.Errorf("%w: mint amount must be non-negative", ErrInvalidEvent)
	}
	// A zero-amount mint is a valid chain no-op: it creates no position and
	// changes no tick, so it must not fail the block.
	if ev.Amount.Sign() == 0 {
		return nil
	}
	if err := validateTickRange(ev.TickLower, ev.TickUpper, state.TickSpacing); err != nil {
		return err
	}

	ensureDeepMaps(deep)
	for i, tick := range []int32{ev.TickLower, ev.TickUpper} {
		// A position outside the known bitmap may already have other liquidity.
		// Keeping only this mint would invent a partial tick when price moves.
		if !tracksTick(deep, tick) {
			continue
		}
		info := loadTick(deep, tick)
		info.LiquidityGross.Add(info.LiquidityGross, ev.Amount)
		if i == 0 {
			info.LiquidityNet.Add(info.LiquidityNet, ev.Amount)
		} else {
			info.LiquidityNet.Sub(info.LiquidityNet, ev.Amount)
		}
		deep.Ticks[tick] = info
		setTickBit(deep.Bitmap, tick, state.TickSpacing)
	}

	liquidity := liquidityOrZero(state.Liquidity)
	if ev.TickLower <= state.Tick && state.Tick < ev.TickUpper {
		liquidity.Add(liquidity, ev.Amount)
	}
	state.Liquidity = liquidity
	return nil
}

func applyBurn(state *pool.State, deep *pool.DeepState, ev Event) error {
	if ev.Amount == nil || ev.Amount.Sign() < 0 {
		return fmt.Errorf("%w: burn amount must be non-negative", ErrInvalidEvent)
	}
	// A zero-amount burn is a valid chain no-op: it removes no liquidity and
	// changes no tick, so it must not fail the block.
	if ev.Amount.Sign() == 0 {
		return nil
	}
	if err := validateTickRange(ev.TickLower, ev.TickUpper, state.TickSpacing); err != nil {
		return err
	}

	// deep may hold only the tick window loaded around the current tick. A burn
	// whose boundary lies outside that window is still a valid chain event, so
	// a missing tick skips its own bookkeeping instead of failing the block.
	// A later dependency refresh can expand the known window if price moves.
	lower, lowerOK := deep.Ticks[ev.TickLower]
	upper, upperOK := deep.Ticks[ev.TickUpper]
	lowerOK = lowerOK && tracksTick(deep, ev.TickLower)
	upperOK = upperOK && tracksTick(deep, ev.TickUpper)
	if lowerOK {
		if !tickLiquidityPresent(lower) {
			return fmt.Errorf("%w: burn tick has nil liquidity", ErrInvalidEvent)
		}
		if lower.LiquidityGross.Cmp(ev.Amount) < 0 {
			return fmt.Errorf("%w: tick %d gross %s below burn %s",
				ErrInsufficientLiquidity, ev.TickLower, lower.LiquidityGross, ev.Amount)
		}
	}
	if upperOK {
		if !tickLiquidityPresent(upper) {
			return fmt.Errorf("%w: burn tick has nil liquidity", ErrInvalidEvent)
		}
		if upper.LiquidityGross.Cmp(ev.Amount) < 0 {
			return fmt.Errorf("%w: tick %d gross %s below burn %s",
				ErrInsufficientLiquidity, ev.TickUpper, upper.LiquidityGross, ev.Amount)
		}
	}

	// Every check runs before the writes below so a rejected burn leaves state
	// and deep untouched.
	liquidity := liquidityOrZero(state.Liquidity)
	inRange := ev.TickLower <= state.Tick && state.Tick < ev.TickUpper
	if inRange && liquidity.Cmp(ev.Amount) < 0 {
		return fmt.Errorf("%w: active liquidity %s below burn %s", ErrInsufficientLiquidity, liquidity, ev.Amount)
	}

	// LiquidityNet is a signed aggregate: a valid tick can hold less net than a
	// single position burning there because other positions may end at the same
	// tick. Only gross bounds the burn; net stays consistent by construction.
	if lowerOK {
		lower.LiquidityGross.Sub(lower.LiquidityGross, ev.Amount)
		lower.LiquidityNet.Sub(lower.LiquidityNet, ev.Amount)
		deep.Ticks[ev.TickLower] = lower
		if lower.LiquidityGross.Sign() == 0 {
			clearTickBit(deep.Bitmap, ev.TickLower, state.TickSpacing)
		}
	}
	if upperOK {
		upper.LiquidityGross.Sub(upper.LiquidityGross, ev.Amount)
		upper.LiquidityNet.Add(upper.LiquidityNet, ev.Amount)
		deep.Ticks[ev.TickUpper] = upper
		if upper.LiquidityGross.Sign() == 0 {
			clearTickBit(deep.Bitmap, ev.TickUpper, state.TickSpacing)
		}
	}

	if inRange {
		liquidity.Sub(liquidity, ev.Amount)
	}
	state.Liquidity = liquidity
	return nil
}

func applySwap(state *pool.State, ev Event) error {
	if ev.SqrtPriceX96 == nil || ev.SqrtPriceX96.Sign() <= 0 {
		return fmt.Errorf("%w: swap sqrt price must be positive", ErrInvalidEvent)
	}
	if ev.Liquidity == nil || ev.Liquidity.Sign() < 0 {
		return fmt.Errorf("%w: swap liquidity must be non-negative", ErrInvalidEvent)
	}
	state.SqrtPriceX96 = new(big.Int).Set(ev.SqrtPriceX96)
	state.Liquidity = new(big.Int).Set(ev.Liquidity)
	state.Tick = ev.Tick
	return nil
}

func ensureDeepMaps(deep *pool.DeepState) {
	if deep.Bitmap == nil {
		deep.Bitmap = make(map[int16]*big.Int)
	}
	if deep.Ticks == nil {
		deep.Ticks = make(map[int32]pool.TickInfo)
	}
}

func validateTickRange(lower, upper, tickSpacing int32) error {
	if lower >= upper {
		return fmt.Errorf("%w: ticks %d..%d", ErrInvalidEvent, lower, upper)
	}
	if tickSpacing <= 0 {
		return fmt.Errorf("%w: tick spacing %d must be positive", ErrInvalidEvent, tickSpacing)
	}
	for _, tick := range [...]int32{lower, upper} {
		if tick < math.MinTick || tick > math.MaxTick {
			return fmt.Errorf("%w: tick %d outside [%d, %d]", ErrInvalidEvent, tick, math.MinTick, math.MaxTick)
		}
		if tick%tickSpacing != 0 {
			return fmt.Errorf("%w: tick %d not aligned to spacing %d", ErrInvalidEvent, tick, tickSpacing)
		}
	}
	return nil
}

// compressTick divides by tickSpacing rounding toward negative infinity, which
// is how V3 maps ticks to bitmap positions.
func compressTick(tick, tickSpacing int32) int32 {
	quotient := tick / tickSpacing
	if tick%tickSpacing != 0 && tick < 0 {
		quotient--
	}
	return quotient
}

// tickBitmapPosition splits a compressed tick into its word and bit index.
// Arithmetic right shift keeps negative compressed ticks in the word below.
func tickBitmapPosition(compressed int32) (int16, int) {
	return int16(compressed >> 8), int(compressed & 0xff)
}

func setTickBit(bitmap map[int16]*big.Int, tick, tickSpacing int32) {
	wordPos, bitPos := tickBitmapPosition(compressTick(tick, tickSpacing))
	word := bitmap[wordPos]
	if word == nil {
		word = new(big.Int)
		bitmap[wordPos] = word
	}
	word.SetBit(word, bitPos, 1)
}

func clearTickBit(bitmap map[int16]*big.Int, tick, tickSpacing int32) {
	wordPos, bitPos := tickBitmapPosition(compressTick(tick, tickSpacing))
	if word := bitmap[wordPos]; word != nil {
		word.SetBit(word, bitPos, 0)
	}
}

func loadTick(deep *pool.DeepState, tick int32) pool.TickInfo {
	info := deep.Ticks[tick]
	if info.LiquidityGross == nil {
		info.LiquidityGross = new(big.Int)
	}
	if info.LiquidityNet == nil {
		info.LiquidityNet = new(big.Int)
	}
	return info
}

func tickLiquidityPresent(info pool.TickInfo) bool {
	return info.LiquidityGross != nil && info.LiquidityNet != nil
}

func liquidityOrZero(v *big.Int) *big.Int {
	if v == nil {
		return new(big.Int)
	}
	return new(big.Int).Set(v)
}

// Unknown windows preserve the reducer's bootstrap behavior. Once a loader has
// declared a complete window, changes outside it must not create partial ticks.
func tracksTick(deep *pool.DeepState, tick int32) bool {
	return deep.WindowMin == 0 && deep.WindowMax == 0 || tick >= deep.WindowMin && tick <= deep.WindowMax
}
