package uniswapv3

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Function selectors of the read-only V3 pool calls used by Load.
const (
	SelectorSlot0      = "0x3850c7bd"
	SelectorLiquidity  = "0x1a686502"
	SelectorTickBitmap = "0x5339c296"
	SelectorTicks      = "0xf30dba93"
)

const abiWordSize = 32

// Spec identifies a V3 pool. Fee and TickSpacing are immutable pool
// parameters that cannot be read through the calls Load makes; the caller
// supplies them from pool creation data.
type Spec struct {
	Address     domain.Address
	Token0      domain.Address
	Token1      domain.Address
	Fee         uint32
	TickSpacing int32
}

// Loaded is the shallow pool state plus the bitmap words and ticks inside the
// requested word radius.
type Loaded struct {
	State pool.State
	Deep  pool.DeepState
}

// Selector constants are package literals; tests assert that each one decodes.
var (
	slot0Call      = decodeSelector(SelectorSlot0)
	liquidityCall  = decodeSelector(SelectorLiquidity)
	tickBitmapCall = decodeSelector(SelectorTickBitmap)
	ticksCall      = decodeSelector(SelectorTicks)
	twoTo256       = new(big.Int).Lsh(big.NewInt(1), 256)
)

// Load reads slot0 and liquidity at block, then walks tickBitmap words in
// [current-wordRadius, current+wordRadius] and reads every initialized tick
// found there. A bitmap word that is missing or empty is treated as zero.
//
// When caller also implements pool.BatchContractCaller the reads are grouped
// into three batches instead of one call each. The returned state is the same
// either way.
//
// Sequential cost is 2 + (2*wordRadius+1) calls, plus one ticks call per set
// bit.
func Load(ctx context.Context, caller pool.ContractCaller, spec Spec, block string, wordRadius int16) (Loaded, error) {
	if err := ctx.Err(); err != nil {
		return Loaded{}, err
	}
	if err := validateSpec(caller, spec, block, wordRadius); err != nil {
		return Loaded{}, err
	}
	if batch, ok := caller.(pool.BatchContractCaller); ok {
		return loadBatched(ctx, batch, spec, block, wordRadius)
	}
	return loadSequential(ctx, caller, spec, block, wordRadius)
}

// LoadShallow reads only slot0 and liquidity at block. It skips tickBitmap and
// ticks, so the returned state has no deep tick data. Use it when the caller
// cannot read historical tick storage or does not quote across ticks.
func LoadShallow(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	if err := ctx.Err(); err != nil {
		return pool.State{}, err
	}
	if err := validateShallowSpec(caller, spec, block); err != nil {
		return pool.State{}, err
	}
	return loadShallowState(ctx, caller, spec, block)
}

func loadSequential(ctx context.Context, caller pool.ContractCaller, spec Spec, block string, wordRadius int16) (Loaded, error) {
	state, err := loadShallowState(ctx, caller, spec, block)
	if err != nil {
		return Loaded{}, err
	}

	deep, err := loadDeepSequential(ctx, caller, spec, block, state.Tick, wordRadius)
	if err != nil {
		return Loaded{}, err
	}
	return Loaded{State: state, Deep: deep}, nil
}

// loadShallowState reads slot0 and liquidity. Callers validate spec and block
// first.
func loadShallowState(ctx context.Context, caller pool.ContractCaller, spec Spec, block string) (pool.State, error) {
	slot0Out, err := callContract(ctx, caller, spec.Address, slot0Call, block, "slot0")
	if err != nil {
		return pool.State{}, err
	}
	sqrtPriceX96, tick, err := parseSlot0(slot0Out)
	if err != nil {
		return pool.State{}, err
	}

	liquidityOut, err := callContract(ctx, caller, spec.Address, liquidityCall, block, "liquidity")
	if err != nil {
		return pool.State{}, err
	}
	liquidity, err := parseLiquidity(liquidityOut)
	if err != nil {
		return pool.State{}, err
	}

	return newLoaded(spec, sqrtPriceX96, tick, liquidity).State, nil
}

// loadDeepSequential reads the bitmap words in [current-wordRadius,
// current+wordRadius] around tick and every initialized tick they hold. It is
// the body Load and DeepLoader.LoadDeep share.
func loadDeepSequential(ctx context.Context, caller pool.ContractCaller, spec Spec, block string, tick int32, wordRadius int16) (pool.DeepState, error) {
	deep := newDeepState()

	// The current word is floor(tick / (tickSpacing*256)). compressTick
	// divides by spacing with floor semantics, and tickBitmapPosition shifts
	// the compressed tick right by 8, which also floors.
	currentWord, _ := tickBitmapPosition(compressTick(tick, spec.TickSpacing))
	firstWord := int64(currentWord) - int64(wordRadius)
	lastWord := int64(currentWord) + int64(wordRadius)
	for position := firstWord; position <= lastWord; position++ {
		wordPos, err := bitmapWordIndex(position)
		if err != nil {
			return pool.DeepState{}, err
		}

		data := callData(tickBitmapCall, encodeSignedWord(position))
		out, err := callContract(ctx, caller, spec.Address, data, block, fmt.Sprintf("tickBitmap(%d)", wordPos))
		if err != nil {
			return pool.DeepState{}, err
		}
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
		deep.Bitmap[wordPos] = word

		ticks, err := initializedTicks(position, word, spec.TickSpacing)
		if err != nil {
			return pool.DeepState{}, err
		}
		for _, initializedTick := range ticks {
			info, err := loadTickInfo(ctx, caller, spec.Address, block, initializedTick)
			if err != nil {
				return pool.DeepState{}, err
			}
			deep.Ticks[initializedTick] = info
		}
	}
	deep.WindowMin, deep.WindowMax = tickWindow(firstWord, lastWord, spec.TickSpacing)
	return deep, nil
}

// tickWindow returns the inclusive tick range covered by bitmap words
// [firstWord, lastWord]. Every word spans 256 compressed ticks, so the raw
// range is word*256*tickSpacing. The bounds are clamped to the int24 tick
// domain the pool can address, which a radius near the end of the range can
// cross.
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

func newDeepState() pool.DeepState {
	return pool.DeepState{
		Bitmap: make(map[int16]*big.Int),
		Ticks:  make(map[int32]pool.TickInfo),
	}
}

func newLoaded(spec Spec, sqrtPriceX96 *big.Int, tick int32, liquidity *big.Int) Loaded {
	return Loaded{
		State: pool.State{
			Key:          domain.PoolKey{Address: spec.Address, Kind: domain.PoolKindV3},
			Token0:       spec.Token0,
			Token1:       spec.Token1,
			Fee:          spec.Fee,
			TickSpacing:  spec.TickSpacing,
			SqrtPriceX96: sqrtPriceX96,
			Tick:         tick,
			Liquidity:    liquidity,
		},
		Deep: newDeepState(),
	}
}

func parseSlot0(out string) (*big.Int, int32, error) {
	values, err := decodeWords("slot0", out, 2)
	if err != nil {
		return nil, 0, err
	}
	sqrtPriceX96, err := unsignedWord("slot0 sqrtPriceX96", values[0], 160)
	if err != nil {
		return nil, 0, err
	}
	rawTick, err := signedWord("slot0 tick", values[1], 24)
	if err != nil {
		return nil, 0, err
	}
	return sqrtPriceX96, int32(rawTick.Int64()), nil
}

func parseLiquidity(out string) (*big.Int, error) {
	values, err := decodeWords("liquidity", out, 1)
	if err != nil {
		return nil, err
	}
	return unsignedWord("liquidity", values[0], 128)
}

func loadTickInfo(ctx context.Context, caller pool.ContractCaller, to domain.Address, block string, tick int32) (pool.TickInfo, error) {
	data := callData(ticksCall, encodeSignedWord(int64(tick)))
	out, err := callContract(ctx, caller, to, data, block, fmt.Sprintf("ticks(%d)", tick))
	if err != nil {
		return pool.TickInfo{}, err
	}
	return parseTickInfo(out)
}

func parseTickInfo(out string) (pool.TickInfo, error) {
	values, err := decodeWords("ticks", out, 2)
	if err != nil {
		return pool.TickInfo{}, err
	}
	gross, err := unsignedWord("ticks liquidityGross", values[0], 128)
	if err != nil {
		return pool.TickInfo{}, err
	}
	net, err := signedWord("ticks liquidityNet", values[1], 128)
	if err != nil {
		return pool.TickInfo{}, err
	}
	return pool.TickInfo{LiquidityGross: gross, LiquidityNet: net}, nil
}

// bitmapWordIndex validates a bitmap word position before any call is made.
func bitmapWordIndex(position int64) (int16, error) {
	if position < -1<<15 || position > 1<<15-1 {
		return 0, fmt.Errorf("%w: bitmap word %d outside int16", ErrInvalidResponse, position)
	}
	return int16(position), nil
}

// initializedTicks returns the ticks set in a non-zero bitmap word, in
// ascending order, rejecting ticks outside int24.
func initializedTicks(position int64, word *big.Int, tickSpacing int32) ([]int32, error) {
	ticks := make([]int32, 0, word.BitLen())
	for bit := 0; bit < 256; bit++ {
		if word.Bit(bit) == 0 {
			continue
		}
		tickValue := (position*256 + int64(bit)) * int64(tickSpacing)
		if tickValue < -1<<23 || tickValue > 1<<23-1 {
			return nil, fmt.Errorf("%w: tick %d outside int24", ErrInvalidResponse, tickValue)
		}
		ticks = append(ticks, int32(tickValue))
	}
	return ticks, nil
}

func validateSpec(caller pool.ContractCaller, spec Spec, block string, wordRadius int16) error {
	if err := validateShallowSpec(caller, spec, block); err != nil {
		return err
	}
	if wordRadius < 1 || wordRadius > 64 {
		return fmt.Errorf("%w: word radius %d outside [1, 64]", ErrInvalidSpec, wordRadius)
	}
	return nil
}

// validateShallowSpec checks the inputs LoadShallow shares with Load. It is
// separate because a shallow load has no word radius.
func validateShallowSpec(caller pool.ContractCaller, spec Spec, block string) error {
	if caller == nil {
		return fmt.Errorf("%w: nil contract caller", ErrInvalidSpec)
	}
	if spec.Address == (domain.Address{}) {
		return fmt.Errorf("%w: zero pool address", ErrInvalidSpec)
	}
	if spec.Token0 == (domain.Address{}) {
		return fmt.Errorf("%w: zero token0", ErrInvalidSpec)
	}
	if spec.Token1 == (domain.Address{}) {
		return fmt.Errorf("%w: zero token1", ErrInvalidSpec)
	}
	if spec.Token0 == spec.Token1 {
		return fmt.Errorf("%w: token0 and token1 are equal", ErrInvalidSpec)
	}
	if spec.Fee >= maxFeePips {
		return fmt.Errorf("%w: fee %d pips outside [0, %d)", ErrInvalidSpec, spec.Fee, maxFeePips)
	}
	if spec.TickSpacing <= 0 {
		return fmt.Errorf("%w: tick spacing %d must be positive", ErrInvalidSpec, spec.TickSpacing)
	}
	if block == "" {
		return fmt.Errorf("%w: empty block", ErrInvalidSpec)
	}
	return nil
}

func callWord(name, out string) (*big.Int, error) {
	values, err := decodeWords(name, out, 1)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(values[0]), nil
}

func callContract(ctx context.Context, caller pool.ContractCaller, to domain.Address, data []byte, block, name string) (string, error) {
	var out string
	if err := caller.CallContract(ctx, to, data, block, &out); err != nil {
		return "", fmt.Errorf("uniswapv3: %s: %w", name, err)
	}
	return out, nil
}

// decodeWords decodes a hex result containing at least minWords 32-byte words.
// Extra words are ignored because slot0 and ticks have fixed ABI shapes but
// forks may append fields.
func decodeWords(name, out string, minWords int) ([][]byte, error) {
	rawHex := strings.TrimPrefix(out, "0x")
	if rawHex == "" {
		return nil, fmt.Errorf("%w: %s returned empty data", ErrInvalidResponse, name)
	}
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return nil, fmt.Errorf("%w: %s is not hex: %v", ErrInvalidResponse, name, err)
	}
	if len(raw) < minWords*abiWordSize || len(raw)%abiWordSize != 0 {
		return nil, fmt.Errorf("%w: %s returned %d bytes, want at least %d whole words",
			ErrInvalidResponse, name, len(raw), minWords)
	}
	values := make([][]byte, len(raw)/abiWordSize)
	for i := range values {
		values[i] = raw[i*abiWordSize : (i+1)*abiWordSize]
	}
	return values, nil
}

// unsignedWord reads an unsigned ABI word and rejects values wider than bits.
func unsignedWord(name string, raw []byte, bits int) (*big.Int, error) {
	value := new(big.Int).SetBytes(raw)
	if value.BitLen() > bits {
		return nil, fmt.Errorf("%w: %s %s does not fit uint%d", ErrInvalidResponse, name, value, bits)
	}
	return value, nil
}

// signedWord reads a two's-complement ABI word and rejects values outside
// int<bits>.
func signedWord(name string, raw []byte, bits int) (*big.Int, error) {
	value := new(big.Int).SetBytes(raw)
	if raw[0]&0x80 != 0 {
		value.Sub(value, twoTo256)
	}
	limit := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
	minimum := new(big.Int).Neg(new(big.Int).Set(limit))
	maximum := new(big.Int).Sub(limit, big.NewInt(1))
	if value.Cmp(minimum) < 0 || value.Cmp(maximum) > 0 {
		return nil, fmt.Errorf("%w: %s %s does not fit int%d", ErrInvalidResponse, name, value, bits)
	}
	return value, nil
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

func callData(selector, argument []byte) []byte {
	data := make([]byte, 0, len(selector)+len(argument))
	data = append(data, selector...)
	data = append(data, argument...)
	return data
}

func decodeSelector(selector string) []byte {
	raw, err := hex.DecodeString(strings.TrimPrefix(selector, "0x"))
	if err != nil {
		return nil
	}
	return raw
}

func isEmptyWord(out string) bool {
	return out == "" || out == "0x"
}
