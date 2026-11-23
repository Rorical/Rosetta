package uniswapv4

import "github.com/Rorical/Rosetta/internal/domain"

// Four-byte event selectors of the V4 PoolManager events. A log's topic0 is keccak256 of the canonical event signature, so its
// first four bytes select the event.
const (
	TopicSwap            = "0x40e9cecb"
	TopicModifyLiquidity = "0xf208f491"
)

// FilterLog reports whether log belongs to a tracked V4 pool: the log address
// is the PoolManager and topic1 is a tracked pool ID. All V4 pools share the
// PoolManager address, so the pool ID is the only discriminator.
//
// topic0 is not checked; the receipt path filters broadly and DecodeSwap
// rejects events other than Swap.
func FilterLog(poolIDs map[domain.Hash]struct{}, poolManager, logAddress domain.Address, topics []domain.Hash) bool {
	if logAddress != poolManager || len(topics) < 2 {
		return false
	}
	_, tracked := poolIDs[topics[1]]
	return tracked
}
