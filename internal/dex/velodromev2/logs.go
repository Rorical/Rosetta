package velodromev2

import (
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Velodrome reserves are uint256. This signature differs from V2's uint112.
var TopicSync, _ = domain.ParseHash("0xcf2aa50876cdfbb541206f89af0ee78d44a2abf8d328e37fa4917f982149848a")

func DecodeSync(topics []domain.Hash, data []byte) (*big.Int, *big.Int, bool, error) {
	if len(topics) == 0 || topics[0] != TopicSync {
		return nil, nil, false, nil
	}
	if len(topics) != 1 || len(data) != 64 {
		return nil, nil, false, fmt.Errorf("%w: Velodrome Sync shape", pool.ErrInvalidState)
	}
	return new(big.Int).SetBytes(data[:32]), new(big.Int).SetBytes(data[32:]), true, nil
}
