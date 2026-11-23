package discovery

import (
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

const (
	abiWordSize = 32
	// addressPadding is the zero prefix in an ABI-encoded address word.
	addressPadding = 12
)

// DecodeV3PoolCreated decodes a Uniswap V3 or Pancake V3 PoolCreated log:
//
//	PoolCreated(address indexed token0, address indexed token1,
//	            uint24 indexed fee, int24 tickSpacing, address pool)
//
// ok is false when topic0 is not the event. A matching event with the wrong
// topic count, data length, or padding returns ErrInvalidLog. The returned
// spec has Kind PoolKindV3; Pancake V3 uses the same engine.
func DecodeV3PoolCreated(log rpc.Log) (Spec, bool, error) {
	if !hasTopic0(log.Topics, topicV3PoolCreated) {
		return Spec{}, false, nil
	}
	if len(log.Topics) != 4 {
		return Spec{}, false, fmt.Errorf("%w: v3 pool created has %d topics, want 4",
			ErrInvalidLog, len(log.Topics))
	}
	if len(log.Data) != 2*abiWordSize {
		return Spec{}, false, fmt.Errorf("%w: v3 pool created data length %d, want %d",
			ErrInvalidLog, len(log.Data), 2*abiWordSize)
	}

	token0, err := addressWord("token0", log.Topics[1][:])
	if err != nil {
		return Spec{}, false, err
	}
	token1, err := addressWord("token1", log.Topics[2][:])
	if err != nil {
		return Spec{}, false, err
	}
	fee, err := uint24Word("fee", log.Topics[3][:])
	if err != nil {
		return Spec{}, false, err
	}
	tickSpacing, err := int24Word("tickSpacing", log.Data[:abiWordSize])
	if err != nil {
		return Spec{}, false, err
	}
	pool, err := addressWord("pool", log.Data[abiWordSize:])
	if err != nil {
		return Spec{}, false, err
	}

	return Spec{
		Address:     pool,
		Token0:      token0,
		Token1:      token1,
		Kind:        domain.PoolKindV3,
		Fee:         fee,
		TickSpacing: tickSpacing,
	}, true, nil
}

// DecodeSlipstreamPoolCreated decodes an Aerodrome Slipstream PoolCreated log:
//
//	PoolCreated(address indexed token0, address indexed token1,
//	            int24 indexed tickSpacing, address pool)
//
// Slipstream's fee is mutable and not part of the event; the returned spec
// leaves Fee zero for the caller to read through fee().
func DecodeSlipstreamPoolCreated(log rpc.Log) (Spec, bool, error) {
	if !hasTopic0(log.Topics, topicSlipstreamPoolCreated) {
		return Spec{}, false, nil
	}
	if len(log.Topics) != 4 {
		return Spec{}, false, fmt.Errorf("%w: slipstream pool created has %d topics, want 4",
			ErrInvalidLog, len(log.Topics))
	}
	if len(log.Data) != abiWordSize {
		return Spec{}, false, fmt.Errorf("%w: slipstream pool created data length %d, want %d",
			ErrInvalidLog, len(log.Data), abiWordSize)
	}

	token0, err := addressWord("token0", log.Topics[1][:])
	if err != nil {
		return Spec{}, false, err
	}
	token1, err := addressWord("token1", log.Topics[2][:])
	if err != nil {
		return Spec{}, false, err
	}
	tickSpacing, err := int24Word("tickSpacing", log.Topics[3][:])
	if err != nil {
		return Spec{}, false, err
	}
	pool, err := addressWord("pool", log.Data)
	if err != nil {
		return Spec{}, false, err
	}

	return Spec{
		Address:     pool,
		Token0:      token0,
		Token1:      token1,
		Kind:        domain.PoolKindSlipstream,
		TickSpacing: tickSpacing,
	}, true, nil
}

// DecodeSolidlyV3PoolCreated decodes a Solidly V3 PoolCreated log:
//
//	PoolCreated(address indexed token0, address indexed token1,
//	            uint24 fee, int24 indexed tickSpacing, address pool)
//
// The signature, and so topic0, matches Uniswap V3, but tickSpacing is the
// indexed field and fee is data. The fee is the creation tier; the pool's
// current fee is read from slot0 when it is loaded.
func DecodeSolidlyV3PoolCreated(log rpc.Log) (Spec, bool, error) {
	if !hasTopic0(log.Topics, topicV3PoolCreated) {
		return Spec{}, false, nil
	}
	if len(log.Topics) != 4 {
		return Spec{}, false, fmt.Errorf("%w: solidly v3 pool created has %d topics, want 4",
			ErrInvalidLog, len(log.Topics))
	}
	if len(log.Data) != 2*abiWordSize {
		return Spec{}, false, fmt.Errorf("%w: solidly v3 pool created data length %d, want %d",
			ErrInvalidLog, len(log.Data), 2*abiWordSize)
	}
	token0, err := addressWord("token0", log.Topics[1][:])
	if err != nil {
		return Spec{}, false, err
	}
	token1, err := addressWord("token1", log.Topics[2][:])
	if err != nil {
		return Spec{}, false, err
	}
	tickSpacing, err := int24Word("tickSpacing", log.Topics[3][:])
	if err != nil {
		return Spec{}, false, err
	}
	fee, err := uint24Word("fee", log.Data[:abiWordSize])
	if err != nil {
		return Spec{}, false, err
	}
	pool, err := addressWord("pool", log.Data[abiWordSize:])
	if err != nil {
		return Spec{}, false, err
	}
	return Spec{Address: pool, Token0: token0, Token1: token1, Kind: domain.PoolKindSolidlyV3, Fee: fee, TickSpacing: tickSpacing}, true, nil
}

// DecodeV4Initialize decodes a PoolManager Initialize log:
//
//	Initialize(bytes32 indexed id, address indexed currency0,
//	           address indexed currency1, uint24 fee, int24 tickSpacing,
//	           address hooks, uint160 sqrtPriceX96, int24 tick)
//
// The spec's Address is the emitting PoolManager and PoolID is topic1. The
// decoder does not check the ID against the key; Discover rejects a mismatch.
func DecodeV4Initialize(log rpc.Log) (Spec, bool, error) {
	if !hasTopic0(log.Topics, topicV4Initialize) {
		return Spec{}, false, nil
	}
	if len(log.Topics) != 4 {
		return Spec{}, false, fmt.Errorf("%w: v4 initialize has %d topics, want 4",
			ErrInvalidLog, len(log.Topics))
	}
	if len(log.Data) != 5*abiWordSize {
		return Spec{}, false, fmt.Errorf("%w: v4 initialize data length %d, want %d",
			ErrInvalidLog, len(log.Data), 5*abiWordSize)
	}

	token0, err := addressWord("currency0", log.Topics[2][:])
	if err != nil {
		return Spec{}, false, err
	}
	token1, err := addressWord("currency1", log.Topics[3][:])
	if err != nil {
		return Spec{}, false, err
	}
	fee, err := uint24Word("fee", log.Data[:abiWordSize])
	if err != nil {
		return Spec{}, false, err
	}
	tickSpacing, err := int24Word("tickSpacing", log.Data[abiWordSize:2*abiWordSize])
	if err != nil {
		return Spec{}, false, err
	}
	hooks, err := addressWord("hooks", log.Data[2*abiWordSize:3*abiWordSize])
	if err != nil {
		return Spec{}, false, err
	}

	return Spec{
		Address:     log.Address,
		Token0:      token0,
		Token1:      token1,
		Kind:        domain.PoolKindV4,
		PoolID:      log.Topics[1],
		Hooks:       hooks,
		Fee:         fee,
		TickSpacing: tickSpacing,
	}, true, nil
}

func hasTopic0(topics []domain.Hash, want domain.Hash) bool {
	return len(topics) > 0 && topics[0] == want
}

// addressWord reads a left-padded 20-byte address from one ABI word.
func addressWord(field string, word []byte) (domain.Address, error) {
	if len(word) != abiWordSize {
		return domain.Address{}, fmt.Errorf("%w: %s word length %d, want %d",
			ErrInvalidLog, field, len(word), abiWordSize)
	}
	for _, b := range word[:addressPadding] {
		if b != 0 {
			return domain.Address{}, fmt.Errorf("%w: %s is not a left-padded address", ErrInvalidLog, field)
		}
	}
	var address domain.Address
	copy(address[:], word[addressPadding:])
	return address, nil
}

// uint24Word reads a uint24 from one ABI word, rejecting a value that would
// not have fit.
func uint24Word(field string, word []byte) (uint32, error) {
	if len(word) != abiWordSize {
		return 0, fmt.Errorf("%w: %s word length %d, want %d",
			ErrInvalidLog, field, len(word), abiWordSize)
	}
	for _, b := range word[:abiWordSize-3] {
		if b != 0 {
			return 0, fmt.Errorf("%w: %s exceeds uint24", ErrInvalidLog, field)
		}
	}
	return uint32(word[29])<<16 | uint32(word[30])<<8 | uint32(word[31]), nil
}

// int24Word reads a sign-extended int24 from one ABI word.
func int24Word(field string, word []byte) (int32, error) {
	if len(word) != abiWordSize {
		return 0, fmt.Errorf("%w: %s word length %d, want %d",
			ErrInvalidLog, field, len(word), abiWordSize)
	}
	value := int32(uint32(word[29])<<16 | uint32(word[30])<<8 | uint32(word[31]))
	negative := value&0x800000 != 0
	padding := byte(0)
	if negative {
		padding = 0xff
	}
	for _, b := range word[:abiWordSize-3] {
		if b != padding {
			return 0, fmt.Errorf("%w: %s is not a sign-extended int24", ErrInvalidLog, field)
		}
	}
	if negative {
		value -= 1 << 24
	}
	return value, nil
}
