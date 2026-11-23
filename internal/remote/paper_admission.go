package remote

import (
	"context"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/pipeline"
	"github.com/Rorical/Rosetta/internal/planning"
)

type paperAdmissionFee struct {
	extra    *big.Int
	maxBytes int
	expires  time.Time
	block    uint64
}

// cachedPaperFee is an early rejection only. A cache miss goes to the pinned
// fee preflight, never to signing or a zero-fee profitability assumption.
func (r *runtime) cachedPaperFee(event arb.SearchEvent, o arb.Opportunity, choice planning.FundingChoice, now time.Time) string {
	p := r.executionPaper
	cache := p.admissionFee.Load()
	if cache == nil || !now.Before(cache.expires) || event.Job.Version.BlockNumber < cache.block || event.Job.Version.BlockNumber-cache.block > 16 {
		return ""
	}
	plan, err := planning.Route(event.Job.View, o, choice.Asset, planning.Policy{ValidSeconds: 10}, big.NewInt(1), r.block.Ref.Timestamp)
	if err != nil {
		return "invalid_route"
	}
	data, err := execution.EncodePackedExecutorPlan(context.Background(), plan)
	if err != nil || len(data) > cache.maxBytes {
		return ""
	}
	base := r.block.BaseFee
	if event.Job.Version.Source == domain.SourceFast && r.flashPrefix.Base.Execution != nil {
		base = r.flashPrefix.Base.Execution.BaseFee
	}
	if base == nil {
		return ""
	}
	tip, ok := new(big.Int).SetString(p.policy.TipWei, 10)
	if !ok {
		return ""
	}
	if tip.Sign() > 0 && planning.HasTransactionFees(o) {
		quoted, _, err := planning.QuoteTransaction(context.Background(), event.Job.View, o, base, tip, 0)
		if err != nil {
			return "transaction_fee_unavailable"
		}
		o = quoted
	}
	fee := new(big.Int).Mul(base, big.NewInt(2))
	fee.Add(fee, tip).Mul(fee, new(big.Int).SetUint64(p.policy.GasLimit)).Add(fee, cache.extra)
	wrapped, _ := domain.ParseAddress(p.policy.WrappedNative)
	economics := p.research.ExecutionEconomics(event.Job.View, o, wrapped, new(big.Int).Sub(o.Net, choice.Asset.MaxLoanFee), choice.Asset.MaxLoanFee, fee)
	if economics.NetworkFee == nil {
		return "fee_conversion_unavailable"
	}
	if _, err = execution.CheckQuotedProfit(o.Net, new(big.Int), choice.Asset.MaxLoanFee, economics.NetworkFee, big.NewInt(1)); err != nil {
		return "network_fee_exceeds_gross"
	}
	return ""
}

// preflightPaperFees checks the complete fee envelope before executor tracing.
// Delayed scenarios remain measurements of the originally selected candidate.
func (p *executionPaper) preflightPaperFees(ctx context.Context, s *snapshot, job paperExecutionJob, plan execution.ExecutorPlan, params execution.CallParameters, result *paperExecutionResult) (bool, error) {
	if job.delayMS != 0 {
		return true, nil
	}
	call, err := execution.BuildExecutorCall(ctx, plan, params)
	if err != nil {
		return false, err
	}
	size, err := execution.OPL1SizeBound(len(call.Data) + 200)
	if err != nil {
		return false, err
	}
	l1, err := s.opFeeWord(ctx, "getL1FeeUpperBound(uint256)", size)
	if err != nil {
		return false, err
	}
	operator, err := s.opFeeWord(ctx, "getOperatorFee(uint256)", call.GasLimit)
	if err != nil {
		return false, err
	}
	extra := new(big.Int).Add(l1, operator)
	// Extra-fee headroom and a short expiry bound reuse between paper groups.
	extra.Mul(extra, big.NewInt(2))
	p.admissionFee.Store(&paperAdmissionFee{extra: new(big.Int).Set(extra), maxBytes: len(call.Data), expires: time.Now().Add(30 * time.Second), block: s.block.Ref.Number})
	fee := new(big.Int).Mul(call.FeeCap, new(big.Int).SetUint64(call.GasLimit))
	fee.Add(fee, extra)
	wrapped, _ := domain.ParseAddress(p.policy.WrappedNative)
	economics := p.research.ExecutionEconomics(job.event.Job.View, job.opportunity, wrapped, new(big.Int).Sub(job.opportunity.Net, plan.MaxLoanFee), plan.MaxLoanFee, fee)
	economics.Basis = "quoted_fee_preflight"
	if economics.NetworkFee == nil {
		result.Status, result.Error = "rejected_fee_unknown", "fee conversion unavailable"
	} else if _, err = execution.CheckQuotedProfit(job.opportunity.Net, new(big.Int), plan.MaxLoanFee, economics.NetworkFee, big.NewInt(1)); err != nil {
		result.Status, result.Error = "rejected_fees", err.Error()
	} else {
		return true, nil
	}
	result.Economics, result.TotalFee = &economics, fee
	return false, nil
}

func candidateTokenRisk(view arb.View, o arb.Opportunity, check func(domain.Address) error) error {
	if err := check(o.Cycle.Start); err != nil {
		return err
	}
	for _, key := range o.Cycle.Pools {
		if p, ok := view.Pool(key); ok {
			if err := pipeline.CheckPoolTokens(p, check); err != nil {
				return err
			}
		}
	}
	return nil
}
