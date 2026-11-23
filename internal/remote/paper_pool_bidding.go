package remote

import (
	"context"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/planning"
)

// Pool-sensitive scenarios reuse measured execution gas, but recompute swap
// output at each bid. They are model counterfactuals, never execution proofs.
func (p *executionPaper) poolBidScenario(ctx context.Context, job paperExecutionJob, result *paperExecutionResult, wrapped domain.Address, baseFee *big.Int, budget *paperBidBudget, gas, tip *big.Int) paperBidScenario {
	scenario := paperBidScenario{Tip: new(big.Int).Set(tip), FitsFeeCap: tip.Cmp(budget.FeeCapLimit) <= 0}
	quoted, _, err := planning.QuoteTransaction(ctx, job.event.Job.View, job.opportunity, baseFee, tip, 0)
	if err != nil {
		scenario.Error = err.Error()
		return scenario
	}
	scenario.PoolGross = quoted.Net
	// Preserve the current simulation's measured residual and loan cost. Only
	// the modeled output change is attributed to the alternative priority fee.
	profit := new(big.Int).Sub(quoted.Net, job.opportunity.Net)
	profit.Add(profit, result.Profit)
	negative := profit.Sign() < 0
	amount := new(big.Int).Abs(profit)
	native := new(big.Int)
	if amount.Sign() > 0 {
		native, _, _ = p.research.NativeValue(job.event.Job.View, job.opportunity.Cycle, wrapped, amount)
		if native == nil {
			scenario.Error = "native valuation unavailable"
			return scenario
		}
		if negative {
			native.Neg(native)
		}
	}
	fees := new(big.Int).Sub(tip, result.Call.TipCap)
	fees.Mul(fees, gas).Add(fees, budget.FeeUpperBound)
	scenario.Net = new(big.Int).Sub(native, fees)
	scenario.Positive = scenario.Net.Sign() > 0
	return scenario
}
