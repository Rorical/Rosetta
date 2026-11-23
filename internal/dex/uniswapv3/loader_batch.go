package uniswapv3

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/pool"
)

// loadBatched performs the same reads as loadSequential in three batch round
// trips: shallow state, bitmap words, then initialized ticks. Bitmap word
// positions depend on the slot0 tick, so they cannot share the shallow batch.
func loadBatched(ctx context.Context, caller pool.BatchContractCaller, spec Spec, block string, wordRadius int16) (Loaded, error) {
	shallow, err := callBatch(ctx, caller, []pool.ContractCall{
		{To: spec.Address, Data: slot0Call, Block: block},
		{To: spec.Address, Data: liquidityCall, Block: block},
	}, "slot0/liquidity")
	if err != nil {
		return Loaded{}, err
	}
	sqrtPriceX96, tick, err := parseSlot0(shallow[0])
	if err != nil {
		return Loaded{}, err
	}
	liquidity, err := parseLiquidity(shallow[1])
	if err != nil {
		return Loaded{}, err
	}

	loaded := newLoaded(spec, sqrtPriceX96, tick, liquidity)
	deep, err := loadDeepBatched(ctx, caller, spec, block, tick, wordRadius)
	if err != nil {
		return Loaded{}, err
	}
	loaded.Deep = deep
	return loaded, nil
}

// loadDeepBatched performs the same reads as loadDeepSequential in two batch
// round trips: bitmap words, then initialized ticks.
func loadDeepBatched(ctx context.Context, caller pool.BatchContractCaller, spec Spec, block string, tick int32, wordRadius int16) (pool.DeepState, error) {
	deep := newDeepState()

	currentWord, _ := tickBitmapPosition(compressTick(tick, spec.TickSpacing))
	width := 2*int(wordRadius) + 1
	positions := make([]int16, 0, width)
	bitmapCalls := make([]pool.ContractCall, 0, width)
	for offset := -int(wordRadius); offset <= int(wordRadius); offset++ {
		position := int64(currentWord) + int64(offset)
		wordPos, err := bitmapWordIndex(position)
		if err != nil {
			return pool.DeepState{}, err
		}
		positions = append(positions, wordPos)
		bitmapCalls = append(bitmapCalls, pool.ContractCall{
			To:    spec.Address,
			Data:  callData(tickBitmapCall, encodeSignedWord(position)),
			Block: block,
		})
	}

	bitmapResults, err := callBatch(ctx, caller, bitmapCalls, "tickBitmap")
	if err != nil {
		return pool.DeepState{}, err
	}
	deep.WindowMin, deep.WindowMax = tickWindow(int64(positions[0]), int64(positions[len(positions)-1]), spec.TickSpacing)
	// A bitmap word cannot produce the same tick twice, so initialized needs no
	// deduplication before the ticks batch.
	var initialized []int32
	for i, out := range bitmapResults {
		if isEmptyWord(out) {
			continue
		}
		word, err := callWord("tickBitmap", out)
		if err != nil {
			return pool.DeepState{}, err
		}
		if word.Sign() == 0 {
			continue
		}
		position := positions[i]
		deep.Bitmap[position] = word

		ticks, err := initializedTicks(int64(position), word, spec.TickSpacing)
		if err != nil {
			return pool.DeepState{}, err
		}
		initialized = append(initialized, ticks...)
	}
	if len(initialized) == 0 {
		return deep, nil
	}

	tickCalls := make([]pool.ContractCall, len(initialized))
	for i, initializedTick := range initialized {
		tickCalls[i] = pool.ContractCall{
			To:    spec.Address,
			Data:  callData(ticksCall, encodeSignedWord(int64(initializedTick))),
			Block: block,
		}
	}
	tickResults, err := callBatch(ctx, caller, tickCalls, "ticks")
	if err != nil {
		return pool.DeepState{}, err
	}
	for i, out := range tickResults {
		info, err := parseTickInfo(out)
		if err != nil {
			return pool.DeepState{}, err
		}
		deep.Ticks[initialized[i]] = info
	}
	return deep, nil
}

func callBatch(ctx context.Context, caller pool.BatchContractCaller, calls []pool.ContractCall, name string) ([]string, error) {
	results, err := caller.CallContracts(ctx, calls)
	if err != nil {
		return nil, fmt.Errorf("uniswapv3: %s batch: %w", name, err)
	}
	if len(results) != len(calls) {
		return nil, fmt.Errorf("%w: %s batch returned %d results for %d calls",
			ErrInvalidResponse, name, len(results), len(calls))
	}
	return results, nil
}
