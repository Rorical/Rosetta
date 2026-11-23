package slipstream

import (
	"fmt"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func topic(signature string) domain.Hash {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(signature))
	var hash domain.Hash
	copy(hash[:], h.Sum(nil))
	return hash
}

var cardinalityTopic = topic("IncreaseObservationCardinalityNext(uint16,uint16)")
var configTopics = func() map[domain.Hash]struct{} {
	result := make(map[domain.Hash]struct{})
	for _, s := range []string{
		"CustomFeeSet(address,uint24)",
		"DefaultFeeCapSet(uint256)",
		"DefaultScalingFactorSet(uint256)",
		"DynamicFeeReset(address)",
		"FeeCapSet(address,uint256)",
		"InitialFeeDisabled(address)",
		"InitialFeeSet(address,uint24)",
		"MevTaxModuleSet(address)",
		"ScalingFactorSet(address,uint256)",
		"SecondsAgoSet(uint32)",
		"SwapFeeModuleChanged(address,address)",
		"DiscountedRegistered(address,uint24)",
		"DiscountedDeregistered(address)",
		"DefaultSwapHookChanged(address)",
		"DiscountRegistryChanged(address)",
		"PoolSwapHookChanged(address,address)",
		"TickSpacingEnabled(int24,uint24)",
		"DiscountSet(address,uint24)",
		"PoolDiscountSet(address,address,uint24)",
		"BaseFeeFactorSet(uint96)",
		"MinThresholdSet(uint96)",
		"MultiplierSet(uint64)",
	} {
		result[topic(s)] = struct{}{}
	}
	return result
}()

// ConfigurationEvent lets the shared receipt reducer avoid scanning pools for
// ordinary swap/transfer logs. Only validated emitters can invalidate a model.
func ConfigurationEvent(topics []domain.Hash) bool {
	if len(topics) == 0 {
		return false
	}
	_, ok := configTopics[topics[0]]
	return ok
}
func AffectsConfiguration(m *pool.SlipstreamState, address domain.Address) bool {
	return m != nil && (address == m.Factory || address == m.Hook || address == m.Registry || address == m.TaxModule)
}

// FollowMetadata handles oracle ring growth emitted by the pool itself.
func FollowMetadata(p *pool.State, topics []domain.Hash, data []byte) (bool, error) {
	if len(topics) == 0 || topics[0] != cardinalityTopic || p.Slipstream == nil {
		return false, nil
	}
	if len(data) != 64 {
		return true, fmt.Errorf("%w: oracle cardinality event", ErrInvalidResponse)
	}
	old, next := wordUint(data[:32]), wordUint(data[32:])
	if old > 65535 || next > 65535 || next < old {
		return true, fmt.Errorf("%w: oracle cardinality range", ErrInvalidResponse)
	}
	if p.Slipstream.Valid && uint16(old) != p.Slipstream.CardinalityNext {
		p.Slipstream.Valid = false
		p.Slipstream.NeedsRefresh = true
		p.Slipstream.UnavailableReason = "cardinality_mismatch"
	}
	p.Slipstream.CardinalityNext = uint16(next)
	return true, nil
}
