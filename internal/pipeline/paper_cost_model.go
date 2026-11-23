package pipeline

import (
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
)

// ProtocolCost records the receipt behind a per-hop cost envelope. The whole
// transaction cost is attributed to each protocol, including fixed overhead;
// this is a conservative observed scenario, not an executor gas quotation.
type ProtocolCost struct {
	Kind         domain.PoolKind `json:"kind"`
	Hops         int             `json:"hops"`
	Samples      int             `json:"samples"`
	NativePerHop *big.Int        `json:"native_per_hop"`
	Transaction  domain.Hash     `json:"transaction"`
	Block        domain.BlockRef `json:"block"`
}

func (b *paperBook) estimateCost(keys []domain.PoolKey, evidence *PaperCalibration) bool {
	shape := paperShape(keys)
	var largest *paidSample
	for i := range b.paid {
		sample := &b.paid[i]
		if sample.shape != shape {
			continue
		}
		evidence.FeeSamples++
		if largest == nil || sample.fees.Total.Cmp(largest.fees.Total) > 0 {
			largest = sample
		}
	}
	if evidence.FeeSamples >= b.policy.MinSamples && largest != nil {
		evidence.CostModel = "matching_transaction_shape"
		evidence.NativeNetworkFee = new(big.Int).Set(largest.fees.Total)
		evidence.SampleTransaction, evidence.SampleBlock = largest.tx, largest.block
		return true
	}
	// Different mixtures can share protocol evidence. Never silently substitute
	// another protocol when a requested kind has no adequately sampled receipts.
	evidence.CostModel = "protocol_hop_envelope"
	evidence.NativeNetworkFee = new(big.Int)
	evidence.FeeSamples = len(b.paid)
	counts := map[domain.PoolKind]int{}
	for _, key := range keys {
		counts[key.Kind]++
	}
	for kind := domain.PoolKindV2; kind <= domain.PoolKindMetricOmm; kind++ {
		if counts[kind] == 0 {
			continue
		}
		component := ProtocolCost{Kind: kind, Hops: counts[kind], NativePerHop: new(big.Int)}
		for _, sample := range b.paid {
			hops := int64(0)
			for _, k := range sample.shape[1 : 1+int(sample.shape[0])] {
				if domain.PoolKind(k) == kind {
					hops++
				}
			}
			if hops == 0 {
				continue
			}
			component.Samples++
			perHop := ceilResearch(sample.fees.Total, hops)
			if component.Samples == 1 || perHop.Cmp(component.NativePerHop) > 0 {
				component.NativePerHop.Set(perHop)
				component.Transaction, component.Block = sample.tx, sample.block
			}
		}
		evidence.ProtocolCosts = append(evidence.ProtocolCosts, component)
		evidence.FeeSamples = min(evidence.FeeSamples, component.Samples)
		evidence.NativeNetworkFee.Add(evidence.NativeNetworkFee, new(big.Int).Mul(component.NativePerHop, big.NewInt(int64(component.Hops))))
	}
	if len(evidence.ProtocolCosts) != len(counts) || evidence.FeeSamples < b.policy.MinSamples {
		evidence.NativeNetworkFee = nil
		return false
	}
	return true
}
