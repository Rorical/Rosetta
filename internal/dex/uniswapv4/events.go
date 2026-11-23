package uniswapv4

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

const modifyLiquidityWordCount = 4

var modifyLiquidityTopic = decodeSelector(TopicModifyLiquidity)

// LiquidityEvent is the state-bearing part of a PoolManager ModifyLiquidity
// log. LiquidityDelta is positive for a mint and negative for a burn; salt is
// ignored because it only distinguishes positions.
type LiquidityEvent struct {
	PoolID         domain.Hash
	TickLower      int32
	TickUpper      int32
	LiquidityDelta *big.Int
}

// DecodeModifyLiquidity decodes one PoolManager ModifyLiquidity log. ok is
// false when topic0 is not ModifyLiquidity. A ModifyLiquidity with a missing
// pool ID topic or malformed data returns ErrInvalidLog.
//
// ModifyLiquidity data is four static words: tickLower, tickUpper,
// liquidityDelta, and salt. Unlike V3 Mint/Burn, the tick bounds are not
// indexed.
func DecodeModifyLiquidity(topics []domain.Hash, data []byte) (LiquidityEvent, bool, error) {
	if len(topics) == 0 {
		return LiquidityEvent{}, false, nil
	}
	if !hasSelector(topics[0], modifyLiquidityTopic) {
		return LiquidityEvent{}, false, nil
	}
	if len(topics) < 2 {
		return LiquidityEvent{}, false, fmt.Errorf("%w: modify liquidity has %d topics, want at least 2",
			ErrInvalidLog, len(topics))
	}
	words, err := modifyLiquidityWords(data)
	if err != nil {
		return LiquidityEvent{}, false, err
	}
	lower, err := tickFromWord("modify liquidity tickLower", words[0])
	if err != nil {
		return LiquidityEvent{}, false, err
	}
	upper, err := tickFromWord("modify liquidity tickUpper", words[1])
	if err != nil {
		return LiquidityEvent{}, false, err
	}
	return LiquidityEvent{
		PoolID:         topics[1],
		TickLower:      lower,
		TickUpper:      upper,
		LiquidityDelta: signedWord256(words[2]),
	}, true, nil
}

func modifyLiquidityWords(data []byte) ([][]byte, error) {
	if len(data) != abiWordSize*modifyLiquidityWordCount {
		return nil, fmt.Errorf("%w: modify liquidity data length %d, want %d",
			ErrInvalidLog, len(data), abiWordSize*modifyLiquidityWordCount)
	}
	words := make([][]byte, modifyLiquidityWordCount)
	for i := range words {
		words[i] = data[i*abiWordSize : (i+1)*abiWordSize]
	}
	return words, nil
}

// DecodeEvent decodes one PoolManager log into the V3-shaped event the state
// reducer applies. Swap becomes EventSwap from the price, liquidity, and tick
// fields; ModifyLiquidity becomes EventMint for a positive delta and EventBurn
// for a negative one, with the amount made positive. A zero delta moves
// nothing, so it becomes EventCollect: recognized but a no-op. ok is false when
// topic0 is neither event; malformed data returns ErrInvalidLog.
func DecodeEvent(topics []domain.Hash, data []byte) (uniswapv3.Event, bool, error) {
	if len(topics) == 0 {
		return uniswapv3.Event{}, false, nil
	}

	switch {
	case hasSelector(topics[0], swapTopic):
		event, ok, err := DecodeSwap(topics, data)
		if err != nil || !ok {
			return uniswapv3.Event{}, ok, err
		}
		return uniswapv3.Event{
			Kind:         uniswapv3.EventSwap,
			SqrtPriceX96: event.SqrtPriceX96,
			Liquidity:    event.Liquidity,
			Tick:         event.Tick,
		}, true, nil

	case hasSelector(topics[0], modifyLiquidityTopic):
		event, ok, err := DecodeModifyLiquidity(topics, data)
		if err != nil || !ok {
			return uniswapv3.Event{}, ok, err
		}
		if event.LiquidityDelta.Sign() == 0 {
			return uniswapv3.Event{Kind: uniswapv3.EventCollect}, true, nil
		}
		kind := uniswapv3.EventMint
		amount := new(big.Int).Set(event.LiquidityDelta)
		if amount.Sign() < 0 {
			kind = uniswapv3.EventBurn
			amount.Neg(amount)
		}
		return uniswapv3.Event{
			Kind:      kind,
			TickLower: event.TickLower,
			TickUpper: event.TickUpper,
			Amount:    amount,
		}, true, nil

	default:
		return uniswapv3.Event{}, false, nil
	}
}

// Apply advances state and deep by one PoolManager log. It reports false for a
// topic it does not handle. Swap and ModifyLiquidity both reduce through the
// V3 reducer: a hooks-free V4 pool moves its price and liquidity on the V3
// curve, and only the fee differs, which is a quote-side concern.
func Apply(state *pool.State, deep *pool.DeepState, topics []domain.Hash, data []byte) (bool, error) {
	if len(topics) == 0 {
		return false, nil
	}
	switch {
	case hasSelector(topics[0], swapTopic):
		return applySwapLog(state, deep, topics, data)
	case hasSelector(topics[0], modifyLiquidityTopic):
		return applyLiquidityLog(state, deep, topics, data)
	default:
		return false, nil
	}
}

func applySwapLog(state *pool.State, deep *pool.DeepState, topics []domain.Hash, data []byte) (bool, error) {
	event, ok, err := DecodeSwap(topics, data)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := validateApplyTargets(state, deep); err != nil {
		return false, err
	}
	ev := uniswapv3.Event{
		Kind:         uniswapv3.EventSwap,
		SqrtPriceX96: event.SqrtPriceX96,
		Liquidity:    event.Liquidity,
		Tick:         event.Tick,
	}
	if err := uniswapv3.Apply(state, deep, ev); err != nil {
		return false, fmt.Errorf("uniswapv4: apply: %w", err)
	}
	return true, nil
}

func applyLiquidityLog(state *pool.State, deep *pool.DeepState, topics []domain.Hash, data []byte) (bool, error) {
	event, ok, err := DecodeModifyLiquidity(topics, data)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	if err := validateApplyTargets(state, deep); err != nil {
		return false, err
	}
	if event.LiquidityDelta.Sign() == 0 {
		// A zero-delta position update moves neither liquidity nor ticks.
		return true, nil
	}

	ev := uniswapv3.Event{
		Kind:      uniswapv3.EventMint,
		TickLower: event.TickLower,
		TickUpper: event.TickUpper,
		Amount:    new(big.Int).Set(event.LiquidityDelta),
	}
	if event.LiquidityDelta.Sign() < 0 {
		ev.Kind = uniswapv3.EventBurn
		ev.Amount.Neg(ev.Amount)
	}
	if err := uniswapv3.Apply(state, deep, ev); err != nil {
		return false, fmt.Errorf("uniswapv4: apply: %w", err)
	}
	return true, nil
}

func validateApplyTargets(state *pool.State, deep *pool.DeepState) error {
	if state == nil || deep == nil {
		return fmt.Errorf("uniswapv4: apply: %w: nil state or deep state", uniswapv3.ErrInvalidEvent)
	}
	return nil
}
