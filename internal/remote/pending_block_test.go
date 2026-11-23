package remote

import (
	"math/big"
	"testing"
	"time"

	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
)

// A route blocked behind the pending transaction is disjoint only when neither
// its swaps nor a standalone flash lender touch the pending route's pools.
func TestPendingBlockRecordsWhetherRoutesShareAPool(t *testing.T) {
	a, b, c, d := domain.PoolKey{Address: domain.Address{1}}, domain.PoolKey{Address: domain.Address{2}}, domain.PoolKey{Address: domain.Address{3}}, domain.PoolKey{Address: domain.Address{4}}
	intent := func(funding execution.Funding, lender domain.PoolKey, pools ...domain.PoolKey) execution.ReactiveIntent {
		var steps []execution.ExecutorStep
		for _, p := range pools {
			steps = append(steps, execution.ExecutorStep{Pool: p})
		}
		i := execution.ReactiveIntent{Route: execution.ExecutorPlan{Funding: funding, Lender: lender, Steps: steps}, Conversion: execution.FeeConversion{Numerator: big.NewInt(1), Denominator: big.NewInt(1)}, ExpectedNet: big.NewInt(5), MinimumNet: big.NewInt(1)}
		i.Call.TipCap = big.NewInt(7)
		// Pools read only to value native profit never block.
		i.Dependencies = append(append([]domain.PoolKey{}, pools...), d)
		return i
	}
	pending := &execution.PendingTransaction{Intent: intent(execution.FundingV3FlashSwap, domain.PoolKey{}, a, b), Nonce: 9}
	now := time.Unix(1793790000, 0)
	cases := []struct {
		name     string
		blocked  execution.ReactiveIntent
		disjoint bool
	}{
		{"separate pools", intent(execution.FundingV3FlashSwap, domain.PoolKey{}, c, d), true},
		{"shared swap pool", intent(execution.FundingV3FlashSwap, domain.PoolKey{}, b, c), false},
		{"lender on the pending route", intent(execution.FundingV3Flash, a, c, d), false},
	}
	for _, tc := range cases {
		got := pendingBlockRecord(pending, tc.blocked, 3, 4, now)
		if got["disjoint"] != tc.disjoint || got["pending_nonce"] != uint64(9) || got["type"] != "pending_blocked" {
			t.Fatalf("%s: %v", tc.name, got)
		}
	}
}
