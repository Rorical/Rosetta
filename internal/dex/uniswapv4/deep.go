package uniswapv4

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Selectors of the StateView tick reads. The getTickBitmap argument is a
// bitmap word position (compressed tick >> 8), not a tick; getTickLiquidity
// takes the tick itself and returns (uint128 liquidityGross, int128
// liquidityNet).
const (
	SelectorGetTickBitmap    = "0x1c7ccb4c"
	SelectorGetTickLiquidity = "0xcaedab54"

	maxWordRadius int16 = 64
)

var (
	getTickBitmapCall    = decodeSelector(SelectorGetTickBitmap)
	getTickLiquidityCall = decodeSelector(SelectorGetTickLiquidity)
	minTickBig           = big.NewInt(int64(math.MinTick))
	maxTickBig           = big.NewInt(int64(math.MaxTick))
)

// DeepLoader loads the tick-level state of a hooks-free V4 pool from StateView.
// It backs pool.DeepLoader for the per-block refresh: the caller passes the
// shallow state it already applied, so no slot0 or liquidity read is repeated
// at the block.
type DeepLoader struct {
	Caller pool.ContractCaller
	// WordRadius is the bitmap word radius used when LoadDeep's wordRadius
	// argument is zero.
	WordRadius int16
}

var _ pool.DeepLoader = (*DeepLoader)(nil)

// LoadDeep reads the bitmap words within wordRadius of state.Tick and every
// initialized tick they contain, in ascending tick order within each word.
//
// Cost: every non-empty bitmap word expands to one getTickLiquidity call per
// initialized tick. A dense pool can hold hundreds of initialized ticks in a
// small window, so one refresh may issue hundreds of eth_calls; the bot batches
// them but sends the batches sequentially. That does not fit a ~300 ms block
// budget on a public endpoint and needs a dedicated RPC or a bulk slot read.
//
// LoadedBlock records the parsed block number when block is a decimal or
// 0x-prefixed hex number. A block tag such as "latest" leaves it zero because
// the resolved number is unknown to this call.
func (l *DeepLoader) LoadDeep(ctx context.Context, state pool.State, block string, wordRadius int16) (pool.DeepState, error) {
	if err := ctx.Err(); err != nil {
		return pool.DeepState{}, err
	}
	if wordRadius == 0 {
		wordRadius = l.WordRadius
	}
	if err := validateDeepSpec(l.Caller, state, block, wordRadius); err != nil {
		return pool.DeepState{}, err
	}

	deep, err := loadDeep(ctx, l.Caller, state, block, wordRadius)
	if err != nil {
		return pool.DeepState{}, err
	}
	if number, ok := parseBlockNumber(block); ok {
		deep.LoadedBlock = domain.BlockRef{Number: number}
	}
	return deep, nil
}

// LoadDeep reads state's tick-level state through a DeepLoader with no
// configured default radius. It is the package-level form used by startup
// seeding, where a loader value would only carry the caller.
func LoadDeep(ctx context.Context, caller pool.ContractCaller, state pool.State, block string, wordRadius int16) (pool.DeepState, error) {
	return (&DeepLoader{Caller: caller}).LoadDeep(ctx, state, block, wordRadius)
}

func validateDeepSpec(caller pool.ContractCaller, state pool.State, block string, wordRadius int16) error {
	if managerFor(caller) == (domain.Address{}) || viewFor(caller) == (domain.Address{}) || state.Key.Address != managerFor(caller) {
		return fmt.Errorf("%w: V4 deployment differs from pool", ErrInvalidSpec)
	}
	if caller == nil {
		return fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if state.Key.Kind != domain.PoolKindV4 {
		return fmt.Errorf("%w: kind %d", ErrUnsupportedKind, state.Key.Kind)
	}
	if state.Key.PoolID == (domain.Hash{}) {
		return fmt.Errorf("%w: zero pool ID", ErrInvalidSpec)
	}
	if state.TickSpacing <= 0 {
		return fmt.Errorf("%w: tick spacing %d must be positive", ErrInvalidSpec, state.TickSpacing)
	}
	if state.Tick < math.MinTick || state.Tick > math.MaxTick {
		return fmt.Errorf("%w: tick %d outside [%d, %d]", ErrInvalidSpec, state.Tick, math.MinTick, math.MaxTick)
	}
	if block == "" {
		return fmt.Errorf("%w: empty block", ErrInvalidSpec)
	}
	if wordRadius < 1 || wordRadius > maxWordRadius {
		return fmt.Errorf("%w: word radius %d outside [1, %d]", ErrInvalidSpec, wordRadius, maxWordRadius)
	}
	return nil
}

// loadDeep reads the bitmap and ticks in two batches when the caller supports
// them and one call at a time otherwise. The returned state is identical.
func loadDeep(ctx context.Context, caller pool.ContractCaller, state pool.State, block string, wordRadius int16) (pool.DeepState, error) {
	positions, err := bitmapPositions(state.Tick, state.TickSpacing, wordRadius)
	if err != nil {
		return pool.DeepState{}, err
	}
	var deep pool.DeepState
	if batch, ok := caller.(pool.BatchContractCaller); ok {
		deep, err = loadDeepBatched(ctx, batch, state, block, positions)
	} else {
		deep, err = loadDeepSequential(ctx, caller, state, block, positions)
	}
	if err != nil {
		return pool.DeepState{}, err
	}
	deep.WindowMin, deep.WindowMax = tickWindow(int64(positions[0]), int64(positions[len(positions)-1]), state.TickSpacing)
	return deep, nil
}

// tickWindow returns the inclusive tick range covered by bitmap words
// [firstWord, lastWord], using the same formula and int24 clamp as V3.
func tickWindow(firstWord, lastWord int64, tickSpacing int32) (int32, int32) {
	minimum := firstWord * 256 * int64(tickSpacing)
	maximum := (lastWord+1)*256*int64(tickSpacing) - 1
	return clampTick(minimum), clampTick(maximum)
}

func clampTick(tick int64) int32 {
	if tick < int64(math.MinTick) {
		return math.MinTick
	}
	if tick > int64(math.MaxTick) {
		return math.MaxTick
	}
	return int32(tick)
}

// bitmapPositions returns the word positions [current-radius, current+radius]
// around tick, where the current word is the compressed tick shifted right by
// eight bits. Tick positions are int16 in the StateView ABI.
func bitmapPositions(tick, tickSpacing int32, wordRadius int16) ([]int16, error) {
	current := floorDiv(tick, tickSpacing) >> 8
	positions := make([]int16, 0, 2*int(wordRadius)+1)
	for offset := -int64(wordRadius); offset <= int64(wordRadius); offset++ {
		position := int64(current) + offset
		if position < -1<<15 || position > 1<<15-1 {
			return nil, fmt.Errorf("%w: bitmap word %d outside int16", ErrInvalidResponse, position)
		}
		positions = append(positions, int16(position))
	}
	return positions, nil
}

func loadDeepSequential(ctx context.Context, caller pool.ContractCaller, state pool.State, block string, positions []int16) (pool.DeepState, error) {
	deep := newDeepState()
	for _, position := range positions {
		word, err := callTickBitmap(ctx, caller, state.Key.PoolID, block, position)
		if err != nil {
			return pool.DeepState{}, err
		}
		if word == nil || word.Sign() == 0 {
			continue
		}
		deep.Bitmap[position] = word

		ticks, err := initializedTicks(position, word, state.TickSpacing)
		if err != nil {
			return pool.DeepState{}, err
		}
		for _, tick := range ticks {
			info, err := callTickLiquidity(ctx, caller, state.Key.PoolID, block, tick)
			if err != nil {
				return pool.DeepState{}, err
			}
			deep.Ticks[tick] = info
		}
	}
	return deep, nil
}

func loadDeepBatched(ctx context.Context, caller pool.BatchContractCaller, state pool.State, block string, positions []int16) (pool.DeepState, error) {
	deep := newDeepState()

	bitmapCalls := make([]pool.ContractCall, len(positions))
	for i, position := range positions {
		bitmapCalls[i] = pool.ContractCall{
			To:    viewFor(caller),
			Data:  stateViewCallData(getTickBitmapCall, state.Key.PoolID, encodeSignedWord(int64(position))),
			Block: block,
		}
	}
	bitmapResults, err := callBatch(ctx, caller, bitmapCalls, "tickBitmap")
	if err != nil {
		return pool.DeepState{}, err
	}

	// A bitmap word cannot produce the same tick twice, and positions are
	// ascending with bits ascending within a word, so ticks stay ordered
	// without deduplication.
	var initialized []int32
	for i, out := range bitmapResults {
		word, err := parseBitmapWord(out)
		if err != nil {
			return pool.DeepState{}, err
		}
		if word == nil || word.Sign() == 0 {
			continue
		}
		deep.Bitmap[positions[i]] = word

		ticks, err := initializedTicks(positions[i], word, state.TickSpacing)
		if err != nil {
			return pool.DeepState{}, err
		}
		initialized = append(initialized, ticks...)
	}
	if len(initialized) == 0 {
		return deep, nil
	}

	tickCalls := make([]pool.ContractCall, len(initialized))
	for i, tick := range initialized {
		tickCalls[i] = pool.ContractCall{
			To:    viewFor(caller),
			Data:  stateViewCallData(getTickLiquidityCall, state.Key.PoolID, encodeSignedWord(int64(tick))),
			Block: block,
		}
	}
	tickResults, err := callBatch(ctx, caller, tickCalls, "tickLiquidity")
	if err != nil {
		return pool.DeepState{}, err
	}
	for i, out := range tickResults {
		info, err := parseTickLiquidity(out)
		if err != nil {
			return pool.DeepState{}, err
		}
		deep.Ticks[initialized[i]] = info
	}
	return deep, nil
}

func newDeepState() pool.DeepState {
	return pool.DeepState{
		Bitmap: make(map[int16]*big.Int),
		Ticks:  make(map[int32]pool.TickInfo),
	}
}
