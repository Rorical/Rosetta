package uniswapv4

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/domain"
)

const (
	abiWordSize      = 32
	swapWordCount    = 6
	topicSelectorLen = 4
)

var (
	swapTopic = decodeSelector(TopicSwap)
	twoTo256  = new(big.Int).Lsh(big.NewInt(1), 256)
)

// SwapEvent is the state-bearing part of a PoolManager Swap log. The V4 Swap
// also carries amount0, amount1, and fee; they are not returned because the
// shallow pool state advances from price, liquidity, and tick only.
type SwapEvent struct {
	PoolID       domain.Hash
	SqrtPriceX96 *big.Int
	Liquidity    *big.Int
	Tick         int32
}

// DecodeSwap decodes one PoolManager Swap log. ok is false when topic0 is not
// Swap. A Swap with a missing pool ID topic or malformed data returns
// ErrInvalidLog.
//
// Swap data is six static words: amount0, amount1, sqrtPriceX96, liquidity,
// tick, fee. The amounts are signed int128 values that are not extracted.
func DecodeSwap(topics []domain.Hash, data []byte) (SwapEvent, bool, error) {
	if len(topics) == 0 {
		return SwapEvent{}, false, nil
	}
	if !hasSelector(topics[0], swapTopic) {
		return SwapEvent{}, false, nil
	}
	if len(topics) < 2 {
		return SwapEvent{}, false, fmt.Errorf("%w: swap has %d topics, want at least 2", ErrInvalidLog, len(topics))
	}

	words, err := swapWords(data)
	if err != nil {
		return SwapEvent{}, false, err
	}
	sqrtPriceX96, err := unsignedWord("swap sqrtPriceX96", words[2], 160)
	if err != nil {
		return SwapEvent{}, false, err
	}
	liquidity, err := unsignedWord("swap liquidity", words[3], 128)
	if err != nil {
		return SwapEvent{}, false, err
	}
	tick, err := tickFromWord("swap tick", words[4])
	if err != nil {
		return SwapEvent{}, false, err
	}
	return SwapEvent{
		PoolID:       topics[1],
		SqrtPriceX96: sqrtPriceX96,
		Liquidity:    liquidity,
		Tick:         tick,
	}, true, nil
}

// swapWords splits the six static Swap words. V4 events use only static types,
// so any other length is malformed.
func swapWords(data []byte) ([][]byte, error) {
	if len(data) != abiWordSize*swapWordCount {
		return nil, fmt.Errorf("%w: swap data length %d, want %d",
			ErrInvalidLog, len(data), abiWordSize*swapWordCount)
	}
	words := make([][]byte, swapWordCount)
	for i := range words {
		words[i] = data[i*abiWordSize : (i+1)*abiWordSize]
	}
	return words, nil
}

// unsignedWord reads an unsigned ABI word and rejects values wider than bits.
func unsignedWord(name string, raw []byte, bits int) (*big.Int, error) {
	value := new(big.Int).SetBytes(raw)
	if value.BitLen() > bits {
		return nil, fmt.Errorf("%w: %s %s does not fit uint%d", ErrInvalidLog, name, value, bits)
	}
	return value, nil
}

// tickFromWord reads a signed int24 ABI word. Indexed and static int24 values
// are sign-extended to 32 bytes, so negative ticks are all-ones-prefixed.
func tickFromWord(name string, raw []byte) (int32, error) {
	value := new(big.Int).SetBytes(raw)
	if raw[0]&0x80 != 0 {
		value.Sub(value, twoTo256)
	}
	if !value.IsInt64() || value.Int64() < -1<<23 || value.Int64() > 1<<23-1 {
		return 0, fmt.Errorf("%w: %s %s outside int24", ErrInvalidLog, name, value)
	}
	return int32(value.Int64()), nil
}

// signedWord256 reads a two's-complement int256 ABI word. Every 256-bit
// pattern is a valid int256.
func signedWord256(raw []byte) *big.Int {
	value := new(big.Int).SetBytes(raw)
	if raw[0]&0x80 != 0 {
		value.Sub(value, twoTo256)
	}
	return value
}

// hasSelector reports whether topic starts with the decoded event selector.
func hasSelector(topic domain.Hash, selector []byte) bool {
	return len(selector) == topicSelectorLen && bytes.Equal(topic[:topicSelectorLen], selector)
}

// decodeSelector decodes a four-byte function or event selector.
func decodeSelector(selector string) []byte {
	raw, err := hex.DecodeString(strings.TrimPrefix(selector, "0x"))
	if err != nil || len(raw) != topicSelectorLen {
		return nil
	}
	return raw
}
