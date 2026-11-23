package remote

import (
	"log/slog"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// One nonce is in flight at a time, so an offer that does not beat the pending
// transaction is dropped even when its pools are disjoint from it and it could
// have taken the next nonce. Such offers are frequent, so each stopped
// offer's best route is recorded to size what parallel nonces would add.

// tradedPools are the pools a transaction swaps or borrows through; pools read
// only to value the profit in native are left out.
func tradedPools(intent execution.ReactiveIntent) []domain.PoolKey {
	pools := planPools(intent.Route)
	if intent.Route.Funding == execution.FundingV3Flash {
		pools = append(pools, intent.Route.Lender)
	}
	return pools
}

func pendingBlockRecord(pending *execution.PendingTransaction, blocked execution.ReactiveIntent, value, pendingValue float64, now time.Time) map[string]any {
	blockedPools, pendingPools := tradedPools(blocked), tradedPools(pending.Intent)
	return map[string]any{
		"type":                    "pending_blocked",
		"at_ms":                   now.UnixMilli(),
		"pending":                 pending.Hash,
		"pending_nonce":           pending.Nonce,
		"pending_pools":           pendingPools,
		"pending_expected_native": pendingValue,
		"blocked_pools":           blockedPools,
		"blocked_expected_native": value,
		"blocked_net_native":      nativeNet(blocked).String(),
		"blocked_tip":             blocked.Call.TipCap,
		"blocked_gas_limit":       blocked.Call.GasLimit,
		"disjoint":                !overlaps(blockedPools, pendingPools),
	}
}

// recordPendingBlock queues the record; a full queue drops it, since it is
// evidence for a design decision, not a nonce fact.
func (s *reactiveRuntime) recordPendingBlock(pending *execution.PendingTransaction, blocked execution.ReactiveIntent, value float64, now time.Time) {
	record := pendingBlockRecord(pending, blocked, value, expectedNative(pending.Intent, s.survival), now)
	if !s.note(record) {
		slog.Warn("pending block record dropped", "pending", pending.Hash.Hex())
	}
}
