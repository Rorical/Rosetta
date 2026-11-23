package pipeline

import (
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
)

// PrefixObservation describes the complete ordered batch visible to the pool
// writer. Index ranges can be joined to canonical receipts without extra RPC.
// It is stream evidence, not an EVM state-root confirmation.
type PrefixObservation struct {
	Stream           string              `json:"stream"`
	Version          domain.StateVersion `json:"version"`
	FrameID          domain.Hash         `json:"frame_id"`
	FrameIndex       uint64              `json:"frame_index"`
	FirstTxIndex     uint64              `json:"first_tx_index"`
	Transactions     []domain.Hash       `json:"transactions"`
	TransactionCount uint64              `json:"transaction_count"`
	DirtyPools       int                 `json:"dirty_pools"`
	ReceivedAtNS     int64               `json:"received_at_ns"`
	AppliedAtNS      int64               `json:"applied_at_ns"`
}

func (r *Research) ObservePrefix(prefix PrefixObservation) {
	if r == nil {
		return
	}
	event := ResearchEvent{Type: "prefix", AtMS: time.Now().UnixMilli(), Version: prefix.Version, Details: prefix}
	select {
	case r.confirmations <- event:
	default:
		r.prefixGaps.add(prefix.ReceivedAtNS)
	}
}
