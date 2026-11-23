package pipeline

import (
	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	"math/big"
)

// ExecutionEconomics values an independently executed sample using local pool
// conversion only. It does not promote simulated gas into paid receipt history.
// Profit is already after loan repayment; adding the loan exposes comparable
// pre-loan gross without deducting it a second time from net.
func (r *Research) ExecutionEconomics(view arb.View, opportunity arb.Opportunity, wrapped domain.Address, profit, loan, nativeFee *big.Int) ResearchEconomics {
	result := ResearchEconomics{Status: NetUnknown, Basis: "executor_simulation", Gross: new(big.Int).Add(profit, loan), LoanFee: new(big.Int).Set(loan)}
	if nativeFee == nil {
		return result
	}
	fee, basis, ok := NativeFeeInRoute(view, opportunity.Cycle, wrapped, nativeFee)
	var pools []domain.PoolKey
	minimum := big.NewInt(1)
	if r != nil && r.book != nil {
		b := r.book
		b.mu.RLock()
		if !ok {
			fee, pools, ok = b.convertOutsideRoute(view, opportunity.Cycle.Start, wrapped, nativeFee)
			basis = "local_pool_spot_scenario"
		}
		if parsed, valid := new(big.Int).SetString(b.policy.MinNetProfit, 10); valid {
			minimum = parsed
		}
		b.mu.RUnlock()
	}
	if !ok {
		return result
	}
	result.NetworkFee = fee
	result.Net = new(big.Int).Sub(profit, fee)
	result.Calibration = &PaperCalibration{ConversionBasis: basis, ConversionPools: pools, NativeNetworkFee: new(big.Int).Set(nativeFee)}
	result.classify(minimum)
	return result
}
