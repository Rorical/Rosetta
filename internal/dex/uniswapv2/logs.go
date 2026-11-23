package uniswapv2

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// TopicSync is topic0 of the V2 Sync event.
var TopicSync = domain.Hash{
	0x1c, 0x41, 0x1e, 0x9a, 0x96, 0xe0, 0x71, 0x24,
	0x1c, 0x2f, 0x21, 0xf7, 0x72, 0x6b, 0x17, 0xae,
	0x89, 0xe3, 0xca, 0xb4, 0xc7, 0x8b, 0xe5, 0x0e,
	0x06, 0x2b, 0x03, 0xa9, 0xff, 0xfb, 0xba, 0xd1,
}

// DecodeSync decodes a Sync log into reserve0 and reserve1. ok is false when
// topic0 is not Sync. Sync has no indexed arguments, so a Sync log with extra
// topics or data of any length other than two words returns ErrInvalidLog.
func DecodeSync(topics []domain.Hash, data []byte) (reserve0, reserve1 *big.Int, ok bool, err error) {
	if len(topics) == 0 || topics[0] != TopicSync {
		return nil, nil, false, nil
	}
	if len(topics) != 1 {
		return nil, nil, false, fmt.Errorf("%w: sync has %d topics, want 1", ErrInvalidLog, len(topics))
	}
	const wordSize = 32
	if len(data) != 2*wordSize {
		return nil, nil, false, fmt.Errorf("%w: sync data length %d, want %d", ErrInvalidLog, len(data), 2*wordSize)
	}
	return new(big.Int).SetBytes(data[:wordSize]), new(big.Int).SetBytes(data[wordSize:]), true, nil
}
