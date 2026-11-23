package uniswapv3

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Event topic signatures. Topic0 of every V3 pool log is one of these.
var (
	TopicInitialize = domain.Hash{
		0x98, 0x63, 0x60, 0x36, 0xcb, 0x66, 0xa9, 0xc1,
		0x9a, 0x37, 0x43, 0x5e, 0xfc, 0x1e, 0x90, 0x14,
		0x21, 0x90, 0x21, 0x4e, 0x8a, 0xbe, 0xb8, 0x21,
		0xbd, 0xba, 0x3f, 0x29, 0x90, 0xdd, 0x4c, 0x95,
	}
	TopicMint = domain.Hash{
		0x7a, 0x53, 0x08, 0x0b, 0xa4, 0x14, 0x15, 0x8b,
		0xe7, 0xec, 0x69, 0xb9, 0x87, 0xb5, 0xfb, 0x7d,
		0x07, 0xde, 0xe1, 0x01, 0xfe, 0x85, 0x48, 0x8f,
		0x08, 0x53, 0xae, 0x16, 0x23, 0x9d, 0x0b, 0xde,
	}
	TopicBurn = domain.Hash{
		0x0c, 0x39, 0x6c, 0xd9, 0x89, 0xa3, 0x9f, 0x44,
		0x59, 0xb5, 0xfa, 0x1a, 0xed, 0x6a, 0x9a, 0x8d,
		0xcd, 0xbc, 0x45, 0x90, 0x8a, 0xcf, 0xd6, 0x7e,
		0x02, 0x8c, 0xd5, 0x68, 0xda, 0x98, 0x98, 0x2c,
	}
	TopicSwap = domain.Hash{
		0xc4, 0x20, 0x79, 0xf9, 0x4a, 0x63, 0x50, 0xd7,
		0xe6, 0x23, 0x5f, 0x29, 0x17, 0x49, 0x24, 0xf9,
		0x28, 0xcc, 0x2a, 0xc8, 0x18, 0xeb, 0x64, 0xfe,
		0xd8, 0x00, 0x4e, 0x11, 0x5f, 0xbc, 0xca, 0x67,
	}
	TopicCollect = domain.Hash{
		0x70, 0x93, 0x53, 0x38, 0xe6, 0x97, 0x75, 0x45,
		0x6a, 0x85, 0xdd, 0xef, 0x22, 0x6c, 0x39, 0x5f,
		0xb6, 0x68, 0xb6, 0x3f, 0xa0, 0x11, 0x5f, 0x5f,
		0x20, 0x61, 0x0b, 0x38, 0x8e, 0x6c, 0xa9, 0xc0,
	}
	// TopicForkMint is the Mint event of a Ramses-style concentrated-liquidity
	// fork (verified on Robinhood factory 0xe0c4ceb9…). It carries the
	// same indexed owner/tickLower/tickUpper as a V3 Mint but inserts an extra
	// data word before the liquidity amount, so its signature hashes
	// differently and V3's TopicMint never matches it. Burn, Swap and Collect
	// match V3 exactly. Real Uniswap V3 pools never emit this topic.
	TopicForkMint = domain.Hash{
		0xd7, 0x82, 0x18, 0xc0, 0xd3, 0x04, 0xe8, 0x89,
		0x3c, 0xb3, 0x20, 0x0a, 0xbe, 0x39, 0x4b, 0xbc,
		0x8d, 0x5b, 0x78, 0x04, 0xd9, 0xc5, 0x1f, 0x23,
		0x6d, 0xf9, 0xfd, 0xcf, 0x48, 0x1d, 0x02, 0xd3,
	}
	// TopicPancakeSwap is Pancake V3's Swap: the V3 fields followed by
	// protocolFeesToken0 and protocolFeesToken1, so its signature hashes
	// differently. Pancake's Mint, Burn and Collect match V3.
	TopicPancakeSwap = domain.Hash{
		0x19, 0xb4, 0x72, 0x79, 0x25, 0x6b, 0x2a, 0x23,
		0xa1, 0x66, 0x5c, 0x81, 0x0c, 0x8d, 0x55, 0xa1,
		0x75, 0x89, 0x40, 0xee, 0x09, 0x37, 0x7d, 0x4f,
		0x8d, 0x26, 0x49, 0x7a, 0x35, 0x77, 0xdc, 0x83,
	}
)

// DecodeLog decodes one V3 pool log into the typed event the state reducer
// consumes. ok is false when topic0 is not a handled V3 event. A handled event
// with the wrong topic count or data length returns ErrInvalidLog.
//
// Mint's sender is not indexed, so its data is four words: sender, amount,
// amount0, amount1. The sender is parsed only as part of the length check and
// is not exposed in Event.
func DecodeLog(topics []domain.Hash, data []byte) (Event, bool, error) {
	if len(topics) == 0 {
		return Event{}, false, nil
	}

	var (
		ev  Event
		err error
	)
	switch topics[0] {
	case TopicInitialize:
		ev, err = decodeInitialize(topics, data)
	case TopicMint:
		ev, err = decodeMint(topics, data)
	case TopicForkMint:
		ev, err = decodeForkMint(topics, data)
	case TopicBurn:
		ev, err = decodeBurn(topics, data)
	case TopicSwap:
		ev, err = decodeSwap(topics, data, 5)
	case TopicPancakeSwap:
		ev, err = decodeSwap(topics, data, 7)
	case TopicCollect:
		ev, err = decodeCollect(topics)
	default:
		return Event{}, false, nil
	}
	if err != nil {
		return Event{}, false, err
	}
	return ev, true, nil
}

func decodeInitialize(topics []domain.Hash, data []byte) (Event, error) {
	if len(topics) != 1 {
		return Event{}, fmt.Errorf("%w: initialize has %d topics, want 1", ErrInvalidLog, len(topics))
	}
	words, err := logWords("initialize", data, 2)
	if err != nil {
		return Event{}, err
	}
	tick, err := tickFromWord("initialize tick", words[1])
	if err != nil {
		return Event{}, err
	}
	return Event{
		Kind:         EventInitialize,
		SqrtPriceX96: new(big.Int).SetBytes(words[0]),
		Tick:         tick,
	}, nil
}

func decodeMint(topics []domain.Hash, data []byte) (Event, error) {
	if len(topics) != 4 {
		return Event{}, fmt.Errorf("%w: mint has %d topics, want 4", ErrInvalidLog, len(topics))
	}
	words, err := logWords("mint", data, 4)
	if err != nil {
		return Event{}, err
	}
	lower, err := tickFromWord("mint tickLower", topics[2][:])
	if err != nil {
		return Event{}, err
	}
	upper, err := tickFromWord("mint tickUpper", topics[3][:])
	if err != nil {
		return Event{}, err
	}
	return Event{
		Kind:      EventMint,
		TickLower: lower,
		TickUpper: upper,
		Amount:    new(big.Int).SetBytes(words[1]),
	}, nil
}

// decodeForkMint reads a Ramses-style fork Mint: indexed owner/tickLower/
// tickUpper like V3, but five data words (sender, an extra word, amount,
// amount0, amount1) with the liquidity amount at index 2 rather than 1. Only
// the amount and the tick bounds advance state; amount0/amount1 are unused, as
// for V3. Verified against the pool's ticks() gross delta.
func decodeForkMint(topics []domain.Hash, data []byte) (Event, error) {
	if len(topics) != 4 {
		return Event{}, fmt.Errorf("%w: fork mint has %d topics, want 4", ErrInvalidLog, len(topics))
	}
	words, err := logWords("fork mint", data, 5)
	if err != nil {
		return Event{}, err
	}
	lower, err := tickFromWord("fork mint tickLower", topics[2][:])
	if err != nil {
		return Event{}, err
	}
	upper, err := tickFromWord("fork mint tickUpper", topics[3][:])
	if err != nil {
		return Event{}, err
	}
	return Event{
		Kind:      EventMint,
		TickLower: lower,
		TickUpper: upper,
		Amount:    new(big.Int).SetBytes(words[2]),
	}, nil
}

func decodeBurn(topics []domain.Hash, data []byte) (Event, error) {
	if len(topics) != 4 {
		return Event{}, fmt.Errorf("%w: burn has %d topics, want 4", ErrInvalidLog, len(topics))
	}
	words, err := logWords("burn", data, 3)
	if err != nil {
		return Event{}, err
	}
	lower, err := tickFromWord("burn tickLower", topics[2][:])
	if err != nil {
		return Event{}, err
	}
	upper, err := tickFromWord("burn tickUpper", topics[3][:])
	if err != nil {
		return Event{}, err
	}
	return Event{
		Kind:      EventBurn,
		TickLower: lower,
		TickUpper: upper,
		Amount:    new(big.Int).SetBytes(words[0]),
	}, nil
}

// decodeSwap reads the five V3 Swap words; Pancake's two trailing protocol
// fee words (count 7) do not change pool state.
func decodeSwap(topics []domain.Hash, data []byte, count int) (Event, error) {
	if len(topics) != 3 {
		return Event{}, fmt.Errorf("%w: swap has %d topics, want 3", ErrInvalidLog, len(topics))
	}
	words, err := logWords("swap", data, count)
	if err != nil {
		return Event{}, err
	}
	tick, err := tickFromWord("swap tick", words[4])
	if err != nil {
		return Event{}, err
	}
	return Event{
		Kind:         EventSwap,
		SqrtPriceX96: new(big.Int).SetBytes(words[2]),
		Liquidity:    new(big.Int).SetBytes(words[3]),
		Tick:         tick,
	}, nil
}

func decodeCollect(topics []domain.Hash) (Event, error) {
	if len(topics) != 4 {
		return Event{}, fmt.Errorf("%w: collect has %d topics, want 4", ErrInvalidLog, len(topics))
	}
	return Event{Kind: EventCollect}, nil
}

// logWords splits fixed-size ABI data into 32-byte words. V3 events use only
// static types, so any other length is malformed.
func logWords(event string, data []byte, count int) ([][]byte, error) {
	const wordSize = 32
	if len(data) != wordSize*count {
		return nil, fmt.Errorf("%w: %s data length %d, want %d", ErrInvalidLog, event, len(data), wordSize*count)
	}
	words := make([][]byte, count)
	for i := range words {
		words[i] = data[i*wordSize : (i+1)*wordSize]
	}
	return words, nil
}

// tickFromWord reads a signed two's-complement word and narrows it to int32.
// Indexed int24 values are sign-extended to 32 bytes, so negative ticks are
// all-ones-prefixed.
func tickFromWord(field string, raw []byte) (int32, error) {
	if len(raw) != 32 {
		return 0, fmt.Errorf("%w: %s word length %d, want 32", ErrInvalidLog, field, len(raw))
	}
	value := new(big.Int).SetBytes(raw)
	if raw[0]&0x80 != 0 {
		value.Sub(value, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	if !value.IsInt64() || value.Int64() < -1<<31 || value.Int64() > 1<<31-1 {
		return 0, fmt.Errorf("%w: %s %s outside int32", ErrInvalidLog, field, value)
	}
	return int32(value.Int64()), nil
}
