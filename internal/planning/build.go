package planning

import (
	"context"
	"fmt"
	"math"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// Batch borrows an immutable pool view and execution snapshot from its caller.
// The caller retains ownership until Build returns.
type Batch struct {
	Version       domain.StateVersion
	Graph         uint64
	View          arb.View
	Snapshot      execution.Snapshot
	Opportunities []arb.Opportunity
	FastProof     *execution.FastReplayEvidence
	FlashProof    *execution.FlashblocksEvidence
}

// Build returns the first fully confirmed candidate in ranking order. Rejected
// candidates consume the supplied batch budget; no extra routes are requested.
func Build(ctx context.Context, batch Batch, cfg Policy, now func() time.Time) (*execution.ReadyPlan, []string, error) {
	var valuer execution.AssetValuer = &Valuer{Reader: batch.Snapshot, Policy: cfg}
	parameters, err := Parameters(ctx, batch.Snapshot, cfg)
	if err != nil {
		return nil, nil, err
	}
	// Native sequencing limits remaining work in the open Fast block. Requiring
	// the whole declared gas limit to fit is conservative across ArbOS versions
	// and does not depend on the unsigned call's estimated poster gas.
	if batch.FastProof != nil && parameters.GasLimit > batch.FastProof.BlockGasLeft {
		return nil, nil, fmt.Errorf("executor gas limit exceeds remaining Fast block gas")
	}
	if batch.FlashProof != nil && parameters.GasLimit > batch.FlashProof.BlockGasLeft {
		return nil, nil, fmt.Errorf("executor gas limit exceeds remaining Flashblocks gas")
	}
	rejected := make([]string, 0)
	metrics, source := telemetry.FromContext(ctx), telemetry.SourceFromContext(ctx)
	reject := func(reason telemetry.Counter, message string) {
		rejected = append(rejected, message)
		metrics.Add(source, reason, 1)
	}
	for _, opportunity := range batch.Opportunities {
		if err := ctx.Err(); err != nil {
			return nil, rejected, err
		}
		asset, allowed := cfg.Assets[opportunity.Cycle.Start]
		if !allowed {
			continue
		}
		if err := checkMetricQuotes(batch.View, opportunity, true, now()); err != nil {
			reject(telemetry.RejectRoute, err.Error())
			continue
		}
		if HasTransactionFees(opportunity) && parameters.TipCap.Sign() > 0 {
			opportunity, _, err = QuoteTransaction(ctx, batch.View, opportunity, batch.Snapshot.Position().BaseFee, parameters.TipCap, 0)
			if err != nil {
				reject(telemetry.RejectFeeEstimate, err.Error())
				continue
			}
		}
		conversion, err := valuer.Conversion(ctx, opportunity.Cycle.Start)
		if err != nil {
			reject(telemetry.RejectOracle, err.Error())
			continue
		}
		route, err := Route(batch.View, opportunity, asset, cfg, new(big.Int).Set(asset.MinNetProfit), batch.Snapshot.Position().Timestamp)
		if err != nil {
			reject(telemetry.RejectRoute, err.Error())
			continue
		}
		budget, err := estimateBudget(ctx, batch.Snapshot, route, parameters)
		if err != nil {
			reject(telemetry.RejectFeeEstimate, err.Error())
			continue
		}
		if budget.Cmp(cfg.MaxTotalFee) > 0 {
			reject(telemetry.RejectFeeCap, "complete fee budget exceeds configured maximum")
			continue
		}
		feeInAsset, err := execution.FeeInAsset(budget, conversion)
		if err != nil {
			reject(telemetry.RejectOracle, err.Error())
			continue
		}
		if _, err := execution.CheckQuotedProfit(opportunity.Net, new(big.Int), asset.MaxLoanFee, feeInAsset, asset.MinNetProfit); err != nil {
			reject(telemetry.RejectFeeCap, err.Error())
			continue
		}
		route.MinProfit = new(big.Int).Add(asset.MinNetProfit, feeInAsset)
		created := now().UnixMilli()
		expires, err := Expiry(created, route.Deadline, cfg.ValidSeconds, conversion)
		if err != nil {
			reject(telemetry.RejectExpiry, err.Error())
			continue
		}
		evidence, err := batch.Snapshot.Confirm(ctx, execution.ConfirmationRequest{Plan: route, Parameters: parameters})
		if err != nil {
			reject(telemetry.RejectSimulation, err.Error())
			continue
		}
		economics, err := execution.CalculateEconomics(*evidence, conversion, asset.MinNetProfit, batch.Snapshot.FeeModel())
		if err != nil {
			reject(telemetry.RejectEconomics, err.Error())
			continue
		}
		plan, err := execution.SealReadyPlan(ctx, execution.ReadyPlan{Version: batch.Version, GraphVersion: batch.Graph,
			PolicyHash: cfg.PolicyHash, Route: route, Evidence: *evidence, Economics: economics, FastReplay: batch.FastProof, Flashblocks: batch.FlashProof,
			CreatedUnixMS: created, ExpiresUnixMS: expires}, batch.Snapshot.FeeModel())
		if err == nil {
			if batch.FlashProof != nil {
				f := batch.FlashProof
				err = execution.ValidateFlashblocksForDispatch(ctx, plan, execution.FlashblocksDispatchContext{Version: batch.Version, PayloadID: f.PayloadID, Index: f.Index, Source: f.Source}, batch.Snapshot.Position().Chain.ChainID, batch.Graph, cfg.PolicyHash, now().UnixMilli(), batch.Snapshot.FeeModel())
			} else if batch.FastProof != nil {
				f := batch.FastProof
				err = execution.ValidateFastForDispatch(ctx, plan, execution.FastDispatchContext{Version: batch.Version,
					ContextSource: f.ContextSource, Generation: f.Generation, ReceiptRevision: f.ReceiptRevision}, batch.Snapshot.Position().Chain.ChainID, batch.Graph, cfg.PolicyHash, now().UnixMilli(), batch.Snapshot.FeeModel())
			} else {
				err = execution.ValidateForDispatch(ctx, plan, batch.Version, batch.Snapshot.Position().Chain.ChainID, batch.Graph, cfg.PolicyHash, now().UnixMilli(), batch.Snapshot.FeeModel())
			}
		}
		if err != nil {
			reject(telemetry.RejectDispatch, err.Error())
			continue
		}
		// One nonce lane can execute one route. Return the first fully qualified
		// ranked candidate immediately instead of waiting for all lower ranks.
		return plan, rejected, nil
	}
	return nil, rejected, ctx.Err()
}

// Parameters binds nonce and the fee envelope to one reader.
func Parameters(ctx context.Context, c execution.StateReader, cfg Policy) (execution.CallParameters, error) {
	position := c.Position()
	if position.BaseFee == nil || position.BaseFee.Sign() <= 0 || position.Chain.ChainID == 0 {
		return execution.CallParameters{}, fmt.Errorf("execution planning requires a chain fee context")
	}
	fee := new(big.Int).Mul(position.BaseFee, new(big.Int).SetUint64(cfg.FeeMultiplier))
	fee.Add(fee, big.NewInt(9999)).Quo(fee, big.NewInt(10_000)).Add(fee, cfg.Tip)
	if fee.Cmp(cfg.MaxFee) > 0 {
		fee.Set(cfg.MaxFee)
	}
	if fee.Cmp(new(big.Int).Add(position.BaseFee, cfg.Tip)) < 0 ||
		new(big.Int).Mul(new(big.Int).SetUint64(cfg.GasLimit), fee).Cmp(cfg.MaxTotalFee) > 0 {
		return execution.CallParameters{}, fmt.Errorf("execution fee envelope exceeds configured budget")
	}
	nonce, err := c.Nonce(ctx, cfg.Sender)
	if err != nil {
		return execution.CallParameters{}, err
	}
	return execution.CallParameters{Sender: cfg.Sender, Executor: cfg.Executor, CodeHash: cfg.CodeHash,
		Nonce: nonce, GasLimit: cfg.GasLimit, FeeCap: fee, TipCap: new(big.Int).Set(cfg.Tip)}, nil
}

// Route preserves the closed token direction and applies per-leg slippage guards.
func Route(view arb.View, opportunity arb.Opportunity, asset AssetPolicy, cfg Policy,
	minimum *big.Int, timestamp uint64) (execution.ExecutorPlan, error) {
	if len(opportunity.Legs) < 2 || len(opportunity.Legs) > 16 || len(opportunity.Legs) != len(opportunity.Cycle.Pools) ||
		opportunity.Input == nil || opportunity.Input.Sign() <= 0 || timestamp > math.MaxInt64/1000-cfg.ValidSeconds {
		return execution.ExecutorPlan{}, fmt.Errorf("invalid candidate route or block timestamp")
	}
	plan := execution.ExecutorPlan{Funding: asset.Funding, Lender: asset.Lender, Asset: opportunity.Cycle.Start,
		Amount: new(big.Int).Set(opportunity.Input), MaxLoanFee: new(big.Int).Set(asset.MaxLoanFee), MinProfit: minimum,
		Deadline: timestamp + cfg.ValidSeconds}
	if asset.Funding == execution.FundingV3Flash {
		lender, ok := view.Pool(asset.Lender)
		if !ok || (lender.Token0 != plan.Asset && lender.Token1 != plan.Asset) {
			return execution.ExecutorPlan{}, fmt.Errorf("flash lender is missing or does not hold the asset")
		}
		plan.LenderToken0 = lender.Token0 == plan.Asset
		if cfg.PoolCheck != nil {
			if err := cfg.PoolCheck(asset.Lender); err != nil {
				return execution.ExecutorPlan{}, err
			}
		}
	}
	current := plan.Asset
	for i, leg := range opportunity.Legs {
		pool, ok := view.Pool(leg.Pool)
		if !ok || leg.Pool != opportunity.Cycle.Pools[i] || leg.AmountOut == nil || leg.AmountOut.Sign() <= 0 {
			return execution.ExecutorPlan{}, fmt.Errorf("candidate leg is missing or differs from cycle")
		}
		if cfg.PoolCheck != nil {
			if err := cfg.PoolCheck(leg.Pool); err != nil {
				return execution.ExecutorPlan{}, err
			}
		}
		if cfg.TokenCheck != nil {
			if err := cfg.TokenCheck(pool.Token0); err != nil {
				return execution.ExecutorPlan{}, err
			}
			if err := cfg.TokenCheck(pool.Token1); err != nil {
				return execution.ExecutorPlan{}, err
			}
		}
		next := pool.Token1
		if current == pool.Token1 {
			next = pool.Token0
		} else if current != pool.Token0 {
			return execution.ExecutorPlan{}, fmt.Errorf("candidate token direction is disconnected")
		}
		minOut := new(big.Int).Mul(leg.AmountOut, new(big.Int).SetUint64(10_000-cfg.Slippage))
		minOut.Quo(minOut, big.NewInt(10_000))
		if minOut.Sign() == 0 {
			minOut.SetInt64(1)
		}
		plan.Steps = append(plan.Steps, execution.ExecutorStep{Pool: leg.Pool, TokenIn: current, TokenOut: next,
			Fee: pool.Fee, TickSpacing: pool.TickSpacing, MinAmountOut: minOut, AmountIn: leg.AmountIn, AmountOut: leg.AmountOut})
		current = next
	}
	if current != plan.Asset {
		return execution.ExecutorPlan{}, fmt.Errorf("candidate does not close in its funding asset")
	}
	return plan, nil
}

// Expiry caps validity at both the execution deadline and oracle freshness.
func Expiry(created int64, deadline, lifetime uint64, conversion execution.FeeConversion) (int64, error) {
	if created <= 0 || deadline > math.MaxInt64/1000 || created > math.MaxInt64-int64(lifetime*1000) {
		return 0, fmt.Errorf("invalid execution-plan wall clock or deadline")
	}
	expires := min(created+int64(lifetime*1000), int64(deadline)*1000)
	for _, price := range []*execution.OracleRound{conversion.NativePrice, conversion.AssetPrice} {
		if price == nil {
			continue
		}
		if conversion.MaxPriceAge > math.MaxInt64/1000 || price.UpdatedAt > math.MaxInt64/1000-conversion.MaxPriceAge {
			return 0, fmt.Errorf("oracle expiry overflows")
		}
		expires = min(expires, int64(price.UpdatedAt+conversion.MaxPriceAge)*1000)
	}
	if expires <= created {
		return 0, fmt.Errorf("snapshot or oracle is too old for a ready plan")
	}
	return expires, nil
}

func estimateBudget(ctx context.Context, snapshot execution.Snapshot, route execution.ExecutorPlan, parameters execution.CallParameters) (*big.Int, error) {
	call, err := execution.BuildExecutorCall(ctx, route, parameters)
	if err != nil {
		return nil, err
	}
	if estimator, ok := snapshot.(execution.FeeBudgetEstimator); ok {
		return estimator.EstimateFeeBudget(ctx, call)
	}
	if snapshot.Position().Chain.Family == domain.FamilyOPStack {
		return nil, fmt.Errorf("OP planning requires a complete state-dependent fee budget")
	}
	return snapshot.FeeModel().Budget(execution.ExecutionEvidence{Call: call})
}
