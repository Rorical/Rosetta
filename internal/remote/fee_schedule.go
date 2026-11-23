package remote

import (
	"github.com/Rorical/Rosetta/internal/bootstrap"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/ingest"
	"github.com/Rorical/Rosetta/internal/pool"
	"time"
)

// canonicalFeeInterval bounds how long a scheduled fee goes unread when no fee
// event appears. It also covers a setter that emits no event, such as a
// Velodrome factory default fee. At five minutes these reads are about 2,900
// calls an hour on OP, nearly all of the follower's traffic; the trade-off is
// that a fee changed without an event is missed for up to an hour.
const canonicalFeeInterval = time.Hour

var (
	velodromeCustomFeeTopic = digest([]byte("SetCustomFee(address,uint256)"))
	v4ProtocolFeeTopic      = digest([]byte("ProtocolFeeUpdated(bytes32,uint24)"))
)

// feeSchedule chooses which dynamic-fee pools the canonical follower rereads.
// Slipstream fees move with trading and keep their per-block reads, which its
// local model already skips. A Velodrome fee changes only by a factory setter,
// an unhooked V4 pool's LP fee never changes and its protocol fee only by a
// PoolManager setter. A Camelot V3 pool logs Fee(uint16,uint16) whenever it
// writes its fee, and the state writer applies it; on Arbitrum One its
// per-block reads were about 19 calls a block. These pools are read
// when first seen, in a block that logs a setter event, and every
// canonicalFeeBlocks. The follower goroutine owns it.
type feeSchedule struct {
	read  map[domain.PoolKey]uint64 // block of the last successful fee read
	every uint64                    // canonicalFeeInterval in blocks
}

func scheduledFee(spec bootstrap.Spec) bool {
	return scheduledFeeKind(spec.Kind, spec.Hooks)
}

// scheduledFeeKind reports a pool whose fee only a setter or the pool's own
// fee event changes.
func scheduledFeeKind(kind domain.PoolKind, hooks domain.Address) bool {
	return kind == domain.PoolKindVelodromeV2 || kind == domain.PoolKindCamelotV3 || kind == domain.PoolKindV4 && hooks == (domain.Address{})
}

// due returns the specs whose fee the follower reads at block.
func (f *feeSchedule) due(specs []bootstrap.Spec, block uint64, transactions []ingest.Transaction) []bootstrap.Spec {
	event := false
	for _, tx := range transactions {
		for _, log := range tx.Logs {
			if len(log.Topics) > 0 && (log.Topics[0] == velodromeCustomFeeTopic || log.Topics[0] == v4ProtocolFeeTopic) {
				event = true
			}
		}
	}
	var out []bootstrap.Spec
	for _, spec := range specs {
		if !scheduledFee(spec) || event {
			out = append(out, spec)
			continue
		}
		last, ok := f.read[domain.PoolKey{Address: spec.Address, Kind: spec.Kind, PoolID: spec.PoolID, Hooks: spec.Hooks}]
		if !ok || block < last || block-last >= f.every {
			out = append(out, spec)
		}
	}
	return out
}

// mark records the fees read at block.
func (f *feeSchedule) mark(states []pool.State, block uint64) {
	if f.read == nil {
		f.read = make(map[domain.PoolKey]uint64)
	}
	for _, p := range states {
		f.read[p.Key] = block
	}
}
