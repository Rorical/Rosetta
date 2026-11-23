package remote

import (
	"context"
	"math/big"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/planning"
)

type paperBidScenario struct {
	Error      string   `json:"error,omitempty"`
	PoolGross  *big.Int `json:"modeled_gross,omitempty"`
	Tip        *big.Int `json:"priority_fee_wei"`
	Net        *big.Int `json:"net_native_wei"`
	FitsFeeCap bool     `json:"fits_fee_cap"`
	Positive   bool     `json:"positive_net"`
}
type paperBidBudget struct {
	Assumption      string             `json:"assumption"`
	Error           string             `json:"error,omitempty"`
	FeeUpperBound   *big.Int           `json:"fee_native_upper_bound,omitempty"`
	Status          string             `json:"status"`
	Basis           string             `json:"valuation_basis"`
	ConversionPools []domain.PoolKey   `json:"conversion_pools,omitempty"`
	Profit          *big.Int           `json:"profit_after_loan_native_wei,omitempty"`
	ZeroTipNet      *big.Int           `json:"zero_tip_net_native_wei,omitempty"`
	EconomicMaxTip  *big.Int           `json:"economic_max_positive_priority_fee_wei,omitempty"`
	MaxPositiveTip  *big.Int           `json:"max_positive_priority_fee_wei,omitempty"`
	FeeCapLimit     *big.Int           `json:"fee_cap_priority_limit_wei,omitempty"`
	Scenarios       []paperBidScenario `json:"scenarios,omitempty"`
}

// Fixed-calldata fee sensitivity holds execution/gas constant. Contracts that
// branch on GASPRICE need a new execution; this is not an inclusion prediction.
func paperBids(profit, fee, gas, currentTip, feeCap, baseFee *big.Int, tips []*big.Int) *paperBidBudget {
	b := &paperBidBudget{Status: "unknown", Basis: "unavailable", Assumption: "fixed_execution_gas; spot_valuation; maximum_tip_encoding_L1_bound; inclusion_unverified"}
	if profit == nil || fee == nil || gas == nil || gas.Sign() <= 0 || currentTip == nil || feeCap == nil || baseFee == nil {
		return b
	}
	b.FeeUpperBound = new(big.Int).Set(fee)
	b.Profit = new(big.Int).Set(profit)
	b.ZeroTipNet = new(big.Int).Sub(profit, fee)
	b.ZeroTipNet.Add(b.ZeroTipNet, new(big.Int).Mul(currentTip, gas))
	b.FeeCapLimit = new(big.Int).Sub(feeCap, baseFee)
	b.Status = "non_positive"
	if b.ZeroTipNet.Sign() > 0 && b.FeeCapLimit.Sign() >= 0 {
		b.MaxPositiveTip = new(big.Int).Sub(b.ZeroTipNet, big.NewInt(1))
		b.MaxPositiveTip.Quo(b.MaxPositiveTip, gas)
		b.EconomicMaxTip = new(big.Int).Set(b.MaxPositiveTip)
		if b.MaxPositiveTip.Cmp(b.FeeCapLimit) > 0 {
			b.MaxPositiveTip.Set(b.FeeCapLimit)
		}
		b.Status = "positive_budget"
	}
	for _, tip := range tips {
		b.Scenarios = append(b.Scenarios, b.atTip(tip, gas))
	}
	return b
}
func (b *paperBidBudget) atTip(tip, gas *big.Int) paperBidScenario {
	net := new(big.Int).Sub(b.ZeroTipNet, new(big.Int).Mul(tip, gas))
	return paperBidScenario{Tip: new(big.Int).Set(tip), Net: net, FitsFeeCap: tip.Cmp(b.FeeCapLimit) <= 0, Positive: net.Sign() > 0}
}
func (p *executionPaper) bidBudget(ctx context.Context, s *snapshot, job paperExecutionJob, result *paperExecutionResult, wrapped domain.Address) *paperBidBudget {
	if result.Status != "success" || result.Profit == nil || result.Call == nil {
		return &paperBidBudget{Status: result.Status, Basis: "unavailable"}
	}
	native, basis, pools := p.research.NativeValue(job.event.Job.View, job.opportunity.Cycle, wrapped, result.Profit)
	if native == nil {
		return &paperBidBudget{Status: "unknown", Basis: basis, Error: "native valuation unavailable"}
	}
	tips := make([]*big.Int, 0, len(p.policy.TipScenariosWei))
	for _, text := range p.policy.TipScenariosWei {
		tip, _ := new(big.Int).SetString(text, 10)
		tips = append(tips, tip)
	}
	gas := new(big.Int).SetUint64(result.GasUsed)
	// A larger priority-fee integer can enlarge the signed transaction. Bound
	// its L1 fee once at the maximum encodable tip under this fixed fee cap.
	fee := new(big.Int).Set(result.TotalFee)
	envelope := *result.Call
	envelope.TipCap = envelope.FeeCap
	quote, err := s.quoteOPFees(ctx, envelope)
	if err != nil {
		return &paperBidBudget{Status: "unknown", Basis: basis, Error: err.Error()}
	}
	if result.Evidence == nil || result.Evidence.OP == nil {
		return &paperBidBudget{Status: "unknown", Basis: basis, Error: "missing original L1 fee evidence"}
	}
	extra := new(big.Int).Sub(quote.proof.L1FeeUpperBound, result.Evidence.OP.L1FeeUpperBound)
	if extra.Sign() > 0 {
		fee.Add(fee, extra)
	}
	b := paperBids(native, fee, gas, result.Call.TipCap, result.Call.FeeCap, s.block.BaseFee, tips)
	b.Basis, b.ConversionPools = basis, pools
	scenarioForTip := func(tip *big.Int) paperBidScenario { return b.atTip(tip, gas) }
	if planning.HasTransactionFees(job.opportunity) {
		reference, _, err := planning.QuoteTransaction(ctx, job.event.Job.View, job.opportunity, s.block.BaseFee, result.Call.TipCap, 0)
		if err != nil {
			return &paperBidBudget{Status: "unknown", Basis: basis, Error: err.Error()}
		}
		job.opportunity = reference
		cache := make(map[string]paperBidScenario)
		scenarioForTip = func(tip *big.Int) paperBidScenario {
			if scenario, ok := cache[tip.String()]; ok {
				return scenario
			}
			scenario := p.poolBidScenario(ctx, job, result, wrapped, s.block.BaseFee, b, gas, tip)
			cache[tip.String()] = scenario
			return scenario
		}
		b.Assumption = "local_transaction_pool_fee; no_account_discount; calibrated_current_gross; fixed_measured_gas; inclusion_unverified"
		b.EconomicMaxTip, b.MaxPositiveTip = nil, nil // No linear break-even formula with pool tax.
		zero := scenarioForTip(new(big.Int))
		b.ZeroTipNet = zero.Net
		b.Error = zero.Error
		b.Scenarios = nil
		b.Status = "modeled_non_positive"
		for _, tip := range tips {
			scenario := scenarioForTip(tip)
			b.Scenarios = append(b.Scenarios, scenario)
			if scenario.Positive && scenario.FitsFeeCap {
				b.Status = "modeled_positive_scenario"
			}
			if scenario.Error != "" {
				b.Status, b.Error = "unknown", scenario.Error
			}
		}
		if b.Error != "" {
			b.Status = "unknown"
		}
	}
	if b.ZeroTipNet != nil && result.Competition != nil {
		for i := range result.Competition.Transactions {
			tx := &result.Competition.Transactions[i]
			if tx.PriorityFee != nil {
				scenario := scenarioForTip(tx.PriorityFee)
				tx.BidScenario = &scenario
			}
		}
	}
	return b
}
