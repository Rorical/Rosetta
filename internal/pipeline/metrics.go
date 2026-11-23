package pipeline

import (
	"hash/fnv"
	"math"
	"math/big"
	"time"

	"github.com/Rorical/Rosetta/internal/arb"
	"github.com/Rorical/Rosetta/internal/domain"
	execution "github.com/Rorical/Rosetta/internal/exec"
	"github.com/Rorical/Rosetta/internal/telemetry"
)

// ObserveDelivered records one plan that left the process. Both backends call
// it so ready counts, latency and estimates mean the same thing everywhere.
func ObserveDelivered(registry *telemetry.Registry, source telemetry.Source, plan *execution.ReadyPlan, receivedAt time.Time) {
	registry.Add(source, telemetry.ReadyPlans, 1)
	if !receivedAt.IsZero() {
		registry.Observe(source, telemetry.ReadyLatency, time.Since(receivedAt))
	}
	if registry == nil {
		return
	}
	registry.Observe(source, telemetry.PlanValidity, time.Duration(plan.ExpiresUnixMS-plan.CreatedUnixMS)*time.Millisecond)
	if updated := oldestPrice(plan.Economics.Conversion); updated > 0 {
		registry.Set(source, telemetry.OracleUpdated, int64(updated))
	}
	if !registry.NewRoute(source, routeFingerprint(plan)) {
		return
	}
	conversion, evidence, economics := plan.Economics.Conversion, plan.Evidence, plan.Economics
	registry.AddEstimate(source, telemetry.EstimateRoutes, 1)
	registry.AddEstimate(source, telemetry.EstimateGrossProfit, assetGwei(evidence.Profit, conversion))
	registry.AddEstimate(source, telemetry.EstimateNetProfit, assetGwei(economics.ActualNetProfit, conversion))
	registry.AddEstimate(source, telemetry.EstimateBudgetNetProfit, assetGwei(economics.BudgetNetProfit, conversion))
	registry.AddEstimate(source, telemetry.EstimateGas, evidence.GasUsed)
	registry.AddEstimate(source, telemetry.EstimateL1Gas, evidence.GasUsedForL1)
	registry.AddEstimate(source, telemetry.EstimateFee, weiToGwei(evidence.TotalFeeWei))
	registry.AddEstimate(source, telemetry.EstimateL1Fee, weiToGwei(evidence.L1FeeWei))
	registry.AddEstimate(source, telemetry.EstimateL2Fee, weiToGwei(evidence.L2FeeWei))
}

// ObserveHeld records a plan the delivery gate stopped, so an operator can see
// what a pause is costing. Held value stays out of the delivered totals.
func ObserveHeld(registry *telemetry.Registry, source telemetry.Source, plan *execution.ReadyPlan) {
	registry.Add(source, telemetry.PlansHeld, 1)
	if !registry.NewRoute(source, routeFingerprint(plan)) {
		return
	}
	registry.AddEstimate(source, telemetry.EstimateHeldRoutes, 1)
	registry.AddEstimate(source, telemetry.EstimateHeldNetProfit, assetGwei(plan.Economics.ActualNetProfit, plan.Economics.Conversion))
}

// routeFingerprint ignores the input amount, which is resized on every block.
func routeFingerprint(plan *execution.ReadyPlan) uint64 {
	hash := fnv.New64a()
	hash.Write(plan.Route.Asset[:])
	for _, step := range plan.Route.Steps {
		hash.Write(step.Pool.Address[:])
		hash.Write([]byte{byte(step.Pool.Kind)})
		hash.Write(step.Pool.PoolID[:])
	}
	return hash.Sum64()
}

func oldestPrice(conversion execution.FeeConversion) uint64 {
	var oldest uint64
	for _, price := range []*execution.OracleRound{conversion.NativePrice, conversion.AssetPrice} {
		if price != nil && (oldest == 0 || price.UpdatedAt < oldest) {
			oldest = price.UpdatedAt
		}
	}
	return oldest
}

// assetGwei inverts the plan's native-to-asset conversion. Losses clamp to zero:
// totals are monotonic counters, and a ready plan is profitable by construction.
func assetGwei(amount *big.Int, conversion execution.FeeConversion) uint64 {
	if amount == nil || amount.Sign() <= 0 || conversion.Numerator == nil || conversion.Numerator.Sign() <= 0 || conversion.Denominator == nil {
		return 0
	}
	wei := new(big.Int).Mul(amount, conversion.Denominator)
	return weiToGwei(wei.Quo(wei, conversion.Numerator))
}

func weiToGwei(wei *big.Int) uint64 {
	if wei == nil || wei.Sign() <= 0 {
		return 0
	}
	gwei := new(big.Int).Quo(wei, big.NewInt(1e9))
	if !gwei.IsUint64() {
		return math.MaxUint64
	}
	return gwei.Uint64()
}

// ObserveTopology publishes the installed search graph and when it was installed.
func ObserveTopology(registry *telemetry.Registry, version uint64, cycles int, truncated bool) {
	registry.Set(telemetry.Standard, telemetry.GraphVersion, int64(version))
	registry.Set(telemetry.Standard, telemetry.GraphInstalled, time.Now().Unix())
	registry.Set(telemetry.Standard, telemetry.GraphCycles, int64(cycles))
	var value int64
	if truncated {
		value = 1
	}
	registry.Set(telemetry.Standard, telemetry.GraphTruncated, value)
}

func MetricSource(source domain.StateSource) telemetry.Source {
	if source == domain.SourceFast {
		return telemetry.Fast
	}
	return telemetry.Standard
}
func searchObserver(registry *telemetry.Registry, source telemetry.Source) func(arb.SearchObservation) {
	if registry == nil {
		return nil
	}
	return func(o arb.SearchObservation) {
		if o.Outcome == arb.SearchContinued {
			registry.Add(source, telemetry.SearchCarriedFrames, 1)
			return
		}
		registry.Add(source, telemetry.SearchPathCanceled, o.PathCanceled)
		registry.Add(source, telemetry.SearchPathReused, o.PathReused)
		registry.Observe(source, telemetry.SearchQueue, o.QueueWait)
		if o.Outcome == arb.SearchSuperseded {
			registry.Add(source, telemetry.SearchSuperseded, 1)
			return
		}
		registry.Add(source, telemetry.SearchFramesAbsorbed, uint64(o.Frames))
		if o.Resumed {
			registry.Add(source, telemetry.SearchResumed, 1)
		}
		registry.Observe(source, telemetry.SearchEvaluate, o.Evaluation)
		registry.Observe(source, telemetry.SearchCatalog, o.Stages.Catalog)
		registry.Observe(source, telemetry.SearchPrepare, o.Stages.Prepare)
		registry.Observe(source, telemetry.SearchRank, o.Stages.Rank)
		registry.Observe(source, telemetry.SearchExactPrepare, o.Stages.ExactPrepare)
		counter := telemetry.SearchCompleted
		if o.Outcome == arb.SearchCanceled {
			counter = telemetry.SearchCanceled
		}
		registry.Add(source, counter, 1)
		if o.Outcome == arb.SearchCompleted {
			if o.Stages.Short > 0 {
				registry.Observe(source, telemetry.SearchShort, o.Stages.Short)
			}
			switch o.Long {
			case arb.LongCompleted:
				registry.Add(source, telemetry.SearchLongCompleted, 1)
				registry.Observe(source, telemetry.SearchLong, o.Stages.Long)
			case arb.LongDeferred:
				registry.Add(source, telemetry.SearchLongDeferred, 1)
			case arb.LongPreempted:
				registry.Add(source, telemetry.SearchLongPreempted, 1)
			}
		}
		switch {
		case o.Overlapped && o.Outcome == arb.SearchCanceled:
			registry.Add(source, telemetry.SearchCanceledOverlap, 1)
		case o.Overtaken:
			registry.Add(source, telemetry.SearchOvertaken, 1)
		}
		if o.Boundary.Kind != arb.BoundaryNone {
			registry.Add(source, [...]telemetry.Counter{telemetry.SearchBoundarySizing, telemetry.SearchBoundaryUntouched, telemetry.SearchBoundaryTouched}[o.Boundary.Reached], 1)
			registry.Add(source, [...]telemetry.Counter{telemetry.SearchBoundaryBlock, telemetry.SearchBoundaryBlock, telemetry.SearchBoundaryAnchor, telemetry.SearchBoundaryEpoch}[o.Boundary.Kind], 1)
		}
		if o.Failed && o.Outcome != arb.SearchCanceled {
			registry.Add(source, telemetry.SearchErrors, 1)
		}
	}
}

func (s *SearchScheduler) observer(job arb.SearchJob) func(arb.SearchObservation) {
	metrics := searchObserver(s.metrics, MetricSource(job.Version.Source))
	sink := s.Observe
	if sink == nil {
		return metrics
	}
	identity := arb.SearchJob{Version: job.Version, GraphVersion: job.GraphVersion, ReceivedAt: job.ReceivedAt}
	return func(observation arb.SearchObservation) {
		if metrics != nil {
			metrics(observation)
		}
		sink(identity, observation)
	}
}
