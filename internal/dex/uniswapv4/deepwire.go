package uniswapv4

import (
	"context"
	"fmt"
	"math/big"
	"strconv"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func callTickBitmap(ctx context.Context, caller pool.ContractCaller, poolID domain.Hash, block string, position int16) (*big.Int, error) {
	out, err := callStateViewWord(ctx, caller, getTickBitmapCall, poolID, encodeSignedWord(int64(position)), block,
		fmt.Sprintf("getTickBitmap(%d)", position))
	if err != nil {
		return nil, err
	}
	return parseBitmapWord(out)
}

func callTickLiquidity(ctx context.Context, caller pool.ContractCaller, poolID domain.Hash, block string, tick int32) (pool.TickInfo, error) {
	out, err := callStateViewWord(ctx, caller, getTickLiquidityCall, poolID, encodeSignedWord(int64(tick)), block,
		fmt.Sprintf("getTickLiquidity(%d)", tick))
	if err != nil {
		return pool.TickInfo{}, err
	}
	return parseTickLiquidity(out)
}

// parseBitmapWord decodes a bitmap word. An empty response is a zero word,
// matching StateView's treatment of an uninitialized position; any other
// response must be at least one whole ABI word.
func parseBitmapWord(out string) (*big.Int, error) {
	if isEmptyWord(out) {
		return nil, nil
	}
	words, err := callWords("getTickBitmap", out, 1)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(words[0]), nil
}

// parseTickLiquidity decodes the ABI order (uint128 liquidityGross, int128
// liquidityNet).
func parseTickLiquidity(out string) (pool.TickInfo, error) {
	words, err := callWords("getTickLiquidity", out, 2)
	if err != nil {
		return pool.TickInfo{}, err
	}
	gross, err := unsignedCallWord("getTickLiquidity liquidityGross", words[0], 128)
	if err != nil {
		return pool.TickInfo{}, err
	}
	net, err := signedBigWord("getTickLiquidity liquidityNet", words[1], 128)
	if err != nil {
		return pool.TickInfo{}, err
	}
	return pool.TickInfo{LiquidityGross: gross, LiquidityNet: net}, nil
}

// signedBigWord reads a two's-complement ABI word and rejects values outside
// int<bits>.
func signedBigWord(name string, raw []byte, bits int) (*big.Int, error) {
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

// initializedTicks returns the ticks set in a non-zero bitmap word, in
// ascending order, rejecting ticks outside int24.
func initializedTicks(position int16, word *big.Int, tickSpacing int32) ([]int32, error) {
	ticks := make([]int32, 0, word.BitLen())
	for bit := 0; bit < 256; bit++ {
		if word.Bit(bit) == 0 {
			continue
		}
		tick, err := bitmapTick(position, bit, tickSpacing)
		if err != nil {
			return nil, err
		}
		ticks = append(ticks, tick)
	}
	return ticks, nil
}

// bitmapTick converts a set bit to its tick. The multiplication runs through
// big.Int because a malformed, very large tick spacing could otherwise
// overflow int64 before the int24 range check rejects it.
func bitmapTick(position int16, bit int, tickSpacing int32) (int32, error) {
	compressed := big.NewInt(int64(position)*256 + int64(bit))
	tick := new(big.Int).Mul(compressed, big.NewInt(int64(tickSpacing)))
	if tick.Cmp(minTickBig) < 0 || tick.Cmp(maxTickBig) > 0 {
		return 0, fmt.Errorf("%w: tick %s outside [%d, %d]", ErrInvalidResponse, tick, math.MinTick, math.MaxTick)
	}
	return int32(tick.Int64()), nil
}

// floorDiv divides by tickSpacing rounding toward negative infinity, which is
// how V4 maps ticks to compressed ticks and bitmap words.
func floorDiv(tick, tickSpacing int32) int32 {
	quotient := tick / tickSpacing
	if tick%tickSpacing != 0 && tick < 0 {
		quotient--
	}
	return quotient
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

func stateViewCallData(selector []byte, poolID domain.Hash, argument []byte) []byte {
	data := make([]byte, 0, len(selector)+len(poolID)+len(argument))
	data = append(data, selector...)
	data = append(data, poolID[:]...)
	data = append(data, argument...)
	return data
}

func callStateViewWord(ctx context.Context, caller pool.ContractCaller, selector []byte, poolID domain.Hash, argument []byte, block, name string) (string, error) {
	var out string
	if err := caller.CallContract(ctx, viewFor(caller), stateViewCallData(selector, poolID, argument), block, &out); err != nil {
		return "", fmt.Errorf("uniswapv4: %s: %w", name, err)
	}
	return out, nil
}

func callBatch(ctx context.Context, caller pool.BatchContractCaller, calls []pool.ContractCall, name string) ([]string, error) {
	results, err := caller.CallContracts(ctx, calls)
	if err != nil {
		return nil, fmt.Errorf("uniswapv4: %s batch: %w", name, err)
	}
	if len(results) != len(calls) {
		return nil, fmt.Errorf("%w: %s batch returned %d results for %d calls",
			ErrInvalidResponse, name, len(results), len(calls))
	}
	return results, nil
}

func isEmptyWord(out string) bool {
	return out == "" || out == "0x"
}

// parseBlockNumber reads the decimal or 0x-prefixed hex block number in block.
// A block tag has no number and reports false.
func parseBlockNumber(block string) (uint64, bool) {
	raw, base := block, 10
	if len(block) > 2 && block[0] == '0' && (block[1] == 'x' || block[1] == 'X') {
		raw, base = block[2:], 16
	}
	number, err := strconv.ParseUint(raw, base, 64)
	if err != nil {
		return 0, false
	}
	return number, true
}
