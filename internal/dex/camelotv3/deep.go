package camelotv3

import (
	"context"
	"fmt"
	"math/big"
	"strconv"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Function selectors of the Camelot V3 (Algebra) deep-state calls. Algebra
// keeps initialized ticks in a tickTable word per raw tick range; the layout
// differs from V3's tickBitmap even though the word size is the same.
const (
	SelectorTickTable = "0xc677e3e0"
	SelectorTicks     = "0xf30dba93"
)

var (
	tickTableCall = decodeSelector(SelectorTickTable)
	ticksCall     = decodeSelector(SelectorTicks)
)

// DeepLoader binds a contract caller to the package LoadDeep function so the
// per-block refresh can hold it as a pool.DeepLoader.
type DeepLoader struct {
	Caller pool.ContractCaller
}

var _ pool.DeepLoader = DeepLoader{}

// LoadDeep reads the Algebra tick table around the current tick and returns a
// V3-shaped deep state for internal/math: Bitmap is keyed by tick compressed by
// TickSpacing, and Ticks is keyed by the raw initialized tick.
//
// Algebra's tickTable is indexed by the raw tick, not by the compressed tick:
// word = tick >> 8, bit = tick & 0xff. This was verified against live Camelot
// factory pools (for example pool
// 0xB1026b8e7276e7AC75410F1fcbbe21796e8f7526, where the compressed word at
// tick -198769 was empty while the raw word held the initialized ticks) and
// against Algebra Integral's TickTree.sol,
// https://github.com/cryptoalgebra/Algebra/blob/integral-v1.2.3/src/core/contracts/libraries/TickTree.sol.
// To fill wordRadius compressed words on each side of the current tick,
// LoadDeep must read every raw word covering those ticks, which costs roughly
// TickSpacing*(2*wordRadius+1) tickTable calls. A missing or empty word is
// treated as zero.
//
// The returned LoadedBlock carries the block number when block is numeric; an
// eth_call tag such as "latest" cannot be resolved to a block reference here,
// so the rest of the reference stays zero.
func LoadDeep(ctx context.Context, caller pool.ContractCaller, state pool.State, block string, wordRadius int16) (pool.DeepState, error) {
	if err := ctx.Err(); err != nil {
		return pool.DeepState{}, err
	}
	if err := validateDeep(caller, state, block, wordRadius); err != nil {
		return pool.DeepState{}, err
	}

	deep := pool.DeepState{
		Bitmap:      make(map[int16]*big.Int),
		Ticks:       make(map[int32]pool.TickInfo),
		LoadedBlock: loadedBlockRef(block),
	}

	currentWord := compressTick(state.Tick, state.TickSpacing) >> 8
	firstWord, lastWord, err := tickTableWordRange(currentWord, int32(wordRadius), state.TickSpacing)
	if err != nil {
		return pool.DeepState{}, err
	}
	deep.WindowMin, deep.WindowMax = tickWindow(
		int64(currentWord)-int64(wordRadius),
		int64(currentWord)+int64(wordRadius),
		state.TickSpacing,
	)

	for position := firstWord; position <= lastWord; position++ {
		name := fmt.Sprintf("tickTable(%d)", position)
		out, err := deepCall(ctx, caller, state.Key.Address, tickTableCall, encodeSignedWord(position), block, name)
		if err != nil {
			return pool.DeepState{}, err
		}
		if isEmptyWord(out) {
			continue
		}
		value, err := callWord("tickTable", out)
		if err != nil {
			return pool.DeepState{}, err
		}
		if value.Sign() == 0 {
			continue
		}

		for bit := 0; bit < 256; bit++ {
			if value.Bit(bit) == 0 {
				continue
			}
			tick, err := rawTick(position, bit, state.TickSpacing)
			if err != nil {
				return pool.DeepState{}, err
			}
			setCompressedTick(&deep, tick, state.TickSpacing)

			info, err := loadTickInfo(ctx, caller, state.Key.Address, block, tick)
			if err != nil {
				return pool.DeepState{}, err
			}
			deep.Ticks[tick] = info
		}
	}
	return deep, nil
}

// LoadDeep implements pool.DeepLoader with the caller bound at construction.
func (l DeepLoader) LoadDeep(ctx context.Context, state pool.State, block string, wordRadius int16) (pool.DeepState, error) {
	return LoadDeep(ctx, l.Caller, state, block, wordRadius)
}

func validateDeep(caller pool.ContractCaller, state pool.State, block string, wordRadius int16) error {
	if caller == nil {
		return fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if state.Key.Address == (domain.Address{}) {
		return fmt.Errorf("%w: zero pool address", ErrInvalidSpec)
	}
	if state.Key.Kind != domain.PoolKindCamelotV3 {
		return fmt.Errorf("%w: pool kind %d is not Camelot V3", ErrInvalidSpec, state.Key.Kind)
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
	if wordRadius < 1 || wordRadius > 64 {
		return fmt.Errorf("%w: word radius %d outside [1, 64]", ErrInvalidSpec, wordRadius)
	}
	return nil
}

func loadTickInfo(ctx context.Context, caller pool.ContractCaller, to domain.Address, block string, tick int32) (pool.TickInfo, error) {
	name := fmt.Sprintf("ticks(%d)", tick)
	out, err := deepCall(ctx, caller, to, ticksCall, encodeSignedWord(int64(tick)), block, name)
	if err != nil {
		return pool.TickInfo{}, err
	}
	return parseTickInfo(out)
}

// parseTickInfo reads the first two words of Algebra's ticks response:
// liquidityTotal (gross) and liquidityDelta (net). Algebra appends six more
// words (fee growth and oracle samples) that the quote path does not use.
func parseTickInfo(out string) (pool.TickInfo, error) {
	values, err := decodeWords("ticks", out, 2)
	if err != nil {
		return pool.TickInfo{}, err
	}
	gross, err := unsignedWord("ticks liquidityTotal", values[0], 128)
	if err != nil {
		return pool.TickInfo{}, err
	}
	net, err := signedWord("ticks liquidityDelta", values[1], 128)
	if err != nil {
		return pool.TickInfo{}, err
	}
	return pool.TickInfo{LiquidityGross: gross, LiquidityNet: net}, nil
}

// compressTick divides by tickSpacing rounding toward negative infinity.
func compressTick(tick, tickSpacing int32) int32 {
	quotient := tick / tickSpacing
	if tick%tickSpacing != 0 && tick < 0 {
		quotient--
	}
	return quotient
}

// tickTableWordRange returns the raw tickTable word range that covers
// compressed words [currentWord-wordRadius, currentWord+wordRadius]. Each
// compressed word spans TickSpacing raw words, so the range is wider than the
// V3 bitmap range for the same radius.
func tickTableWordRange(currentWord, wordRadius, tickSpacing int32) (int64, int64, error) {
	firstCompressedWord := int64(currentWord) - int64(wordRadius)
	lastCompressedWord := int64(currentWord) + int64(wordRadius)
	firstWord := firstCompressedWord * int64(tickSpacing)
	lastWord := floorDiv64((lastCompressedWord*256+255)*int64(tickSpacing), 256)

	if firstWord < -1<<15 || firstWord > 1<<15-1 || lastWord < -1<<15 || lastWord > 1<<15-1 {
		return 0, 0, fmt.Errorf("%w: tick table word range [%d, %d] outside int16",
			ErrInvalidResponse, firstWord, lastWord)
	}
	return firstWord, lastWord, nil
}

// rawTick converts a bit in a raw tickTable word to the raw tick, rejecting
// ticks outside int24 and ticks that do not belong to this pool's spacing. An
// unaligned tick means the supplied TickSpacing does not match the pool.
func rawTick(wordPosition int64, bit int, tickSpacing int32) (int32, error) {
	tick := wordPosition*256 + int64(bit)
	if tick < -1<<23 || tick > 1<<23-1 {
		return 0, fmt.Errorf("%w: tick %d outside int24", ErrInvalidResponse, tick)
	}
	if tick%int64(tickSpacing) != 0 {
		return 0, fmt.Errorf("%w: tick %d is not a multiple of spacing %d", ErrInvalidResponse, tick, tickSpacing)
	}
	return int32(tick), nil
}

// setCompressedTick adds tick to the V3-shaped bitmap, where positions are
// indexed by tick divided by TickSpacing.
func setCompressedTick(deep *pool.DeepState, tick, tickSpacing int32) {
	compressed := tick / tickSpacing
	wordPosition := int16(compressed >> 8)
	word := deep.Bitmap[wordPosition]
	if word == nil {
		word = new(big.Int)
		deep.Bitmap[wordPosition] = word
	}
	word.SetBit(word, int(compressed&0xff), 1)
}

func floorDiv64(a, b int64) int64 {
	quotient := a / b
	if a%b != 0 && (a < 0) != (b < 0) {
		quotient--
	}
	return quotient
}

// tickWindow returns the inclusive tick range covered by compressed bitmap
// words [firstWord, lastWord]. Algebra stores ticks in a raw tick table, but
// the quote path consumes the V3-shaped compressed bitmap, so the window uses
// the same word*256*tickSpacing formula as V3. The bounds are clamped to the
// int24 tick domain.
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

func callWord(name, out string) (*big.Int, error) {
	values, err := decodeWords(name, out, 1)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(values[0]), nil
}

func deepCall(ctx context.Context, caller pool.ContractCaller, to domain.Address, selector, argument []byte, block, name string) (string, error) {
	data := make([]byte, 0, len(selector)+len(argument))
	data = append(data, selector...)
	data = append(data, argument...)
	var out string
	if err := caller.CallContract(ctx, to, data, block, &out); err != nil {
		return "", fmt.Errorf("camelotv3: %s: %w", name, err)
	}
	return out, nil
}

// encodeSignedWord returns the 32-byte two's-complement encoding of value, the
// static ABI form of int16 and int24 arguments.
func encodeSignedWord(value int64) []byte {
	encoded := big.NewInt(value)
	if value < 0 {
		encoded.Add(encoded, twoTo256)
	}
	word := make([]byte, abiWordSize)
	encoded.FillBytes(word)
	return word
}

func isEmptyWord(out string) bool {
	return out == "" || out == "0x"
}

// loadedBlockRef records the numeric block tag. Hash, parent, timestamp, and
// L1 height are not available from an eth_call block tag and stay zero.
func loadedBlockRef(block string) domain.BlockRef {
	base := 10
	raw := block
	if strings.HasPrefix(block, "0x") || strings.HasPrefix(block, "0X") {
		base = 16
		raw = block[2:]
	}
	number, err := strconv.ParseUint(raw, base, 64)
	if err != nil {
		return domain.BlockRef{}
	}
	return domain.BlockRef{Number: number}
}
