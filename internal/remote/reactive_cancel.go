package remote

import (
	"context"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/graph"
)

// cancelContextLag is how far the reactive context may trail the version a
// cancellation is priced at: two OP blocks.
const cancelContextLag = 4 * time.Second

// cancelSizeBound bounds a signed zero-value self-transfer: an EIP-1559
// envelope with empty calldata is about 110 bytes.
const cancelSizeBound = 200

func pendingDependencies(p execution.PendingTransaction) []domain.PoolKey {
	keys := append([]domain.PoolKey{}, p.Intent.Dependencies...)
	for _, step := range p.Intent.Route.Steps {
		keys = append(keys, step.Pool)
	}
	if p.Intent.Route.Funding == execution.FundingV3Flash {
		keys = append(keys, p.Intent.Route.Lender)
	}
	for _, old := range p.Previous {
		keys = append(keys, pendingDependencies(old)...)
	}
	return keys
}

// A swap by any trader can consume an opportunity. Identity matching is not
// needed: only changes to the live variants' dependencies trigger re-quotation.
func (r *runtime) considerReactiveCancellation(ctx context.Context, view arb.View, dirty []domain.PoolKey, version domain.StateVersion) {
	s := r.reactive
	if r.catchingUp && !r.nearTip {
		return
	}
	if s == nil || s.dispatcher == nil || s.settings.document.CancelFailureGas == 0 || s.dispatcher.Included() {
		return
	}
	p := s.dispatcher.Pending()
	if p == nil || p.Cancellation != nil {
		return
	}
	keys := pendingDependencies(*p)
	affected := false
	for _, key := range keys {
		for _, changed := range dirty {
			affected = affected || key == changed
		}
	}
	if !affected {
		return
	}
	cache := s.cache.Load()
	now := time.Now()
	if cache == nil || cache.costs.ExpiresMS <= now.UnixMilli() || cache.block.Number > version.Anchor.Number || version.Anchor.Number-cache.block.Number > r.cfg.Network.Blocks(cancelContextLag) {
		return
	}
	if version.Source == domain.SourceStandard && r.streamActive(time.Now()) {
		return
	}
	var failureCost *big.Int
	variants := append([]execution.PendingTransaction{*p}, p.Previous...)
	for _, variant := range variants {
		lost, dependencies := r.remainingReactiveLoss(ctx, view, variant.Intent, cache)
		if !lost {
			return
		} // Missing quote evidence is not evidence of a loss.
		keys = append(keys, dependencies...)
		gas := min(s.settings.document.CancelFailureGas, variant.Intent.Call.GasLimit)
		// Expired executor calls fail early; avoid pricing them as full-route swaps.
		if uint64(now.Unix()) > variant.Intent.Route.Deadline {
			gas = min(gas, 30000)
		}
		price := new(big.Int).Add(cache.costs.BaseFee, variant.Intent.Call.TipCap)
		if price.Cmp(variant.Intent.Call.FeeCap) > 0 {
			price.Set(variant.Intent.Call.FeeCap)
		}
		cost := new(big.Int).Add(new(big.Int).Mul(price, new(big.Int).SetUint64(gas)), cache.costs.Extra)
		if failureCost == nil || cost.Cmp(failureCost) < 0 {
			failureCost = cost
		}
	}
	tip := execution.ReplacementFee(p.Intent.Call.TipCap)
	cap := execution.ReplacementFee(p.Intent.Call.FeeCap)
	base := new(big.Int).Add(new(big.Int).Mul(cache.costs.BaseFee, big.NewInt(2)), tip)
	if base.Cmp(cap) > 0 {
		cap = base
	}
	cfg := s.settings.planning
	if cap.Cmp(cfg.MaxFee) > 0 || tip.Cmp(cfg.MaxTip) > 0 {
		return
	}
	// On Nitro the self-transfer pays poster gas too, inside its limit.
	gas := s.settings.document.CancelGasLimit + cache.costs.PosterGasAt(cancelSizeBound)
	cost := new(big.Int).Add(new(big.Int).Mul(cap, new(big.Int).SetUint64(gas)), cache.costs.Extra)
	if cost.Cmp(cfg.MaxTotalFee) > 0 || cost.Cmp(cache.native) > 0 || cost.Cmp(failureCost) >= 0 {
		return
	}
	c := execution.Cancellation{Target: p.Hash, FeeCap: cap, TipCap: tip, GasLimit: gas, Budget: cost, EstimatedFailureFee: failureCost, CreatedMS: now.UnixMilli(), ExpiresMS: min(now.Add(cfg.Lifetime).UnixMilli(), cache.costs.ExpiresMS), Reason: "remaining_profit_below_guard"}
	guard := r.reactiveGuard(keys, cache, r.reactiveRevision.Load(), r.appliedRevision)
	if guard == nil {
		return
	}
	s.offerJob(reactiveJob{intent: p.Intent, current: guard, cancellation: &c, target: p.Hash})
}

func (r *runtime) remainingReactiveLoss(ctx context.Context, view arb.View, i execution.ReactiveIntent, cache *reactiveCache) (bool, []domain.PoolKey) {
	cycle := graph.Cycle{Start: i.Route.Asset, Len: len(i.Route.Steps)}
	for _, step := range i.Route.Steps {
		cycle.Pools = append(cycle.Pools, step.Pool)
	}
	quote, err := r.topology.Engine.QuoteCycle(ctx, view, cycle, i.Route.Amount)
	if err != nil || quote.Net == nil {
		return false, nil
	}
	for index, leg := range quote.Legs {
		if leg.AmountOut.Cmp(i.Route.Steps[index].MinAmountOut) < 0 {
			return true, nil
		}
	}
	loan := new(big.Int)
	if i.Route.Funding == execution.FundingV3Flash {
		lender, ok := view.Pool(i.Route.Lender)
		if !ok {
			return false, nil
		}
		loan.Mul(i.Route.Amount, new(big.Int).SetUint64(uint64(lender.Fee)))
		loan.Add(loan, big.NewInt(999999)).Quo(loan, big.NewInt(1000000))
	}
	afterLoan := new(big.Int).Sub(quote.Net, loan)
	// The original calldata's guards remain authoritative even if a newly
	// admitted route could use smaller limits against the changed pool state.
	if loan.Cmp(i.Route.MaxLoanFee) > 0 || afterLoan.Cmp(i.Route.MinProfit) < 0 {
		return true, nil
	}
	unit := new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)
	native, _, keys := r.research.NativeValue(view, cycle, r.reactive.settings.wrapped, unit)
	if native == nil || native.Sign() <= 0 {
		return false, nil
	}
	nativeCost := new(big.Int).Add(new(big.Int).Mul(i.Call.FeeCap, new(big.Int).SetUint64(i.Call.GasLimit)), cache.costs.Extra)
	cost, err := execution.FeeInAsset(nativeCost, execution.FeeConversion{Numerator: unit, Denominator: native})
	if err != nil {
		return false, nil
	}
	return new(big.Int).Sub(afterLoan, cost).Cmp(i.MinimumNet) < 0, keys
}
