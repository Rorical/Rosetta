package exec

import (
	"bytes"
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
)

// Pool kinds of a chained hot route's step word (contracts/src/HotV3Executor.sol).
const (
	chainPool   = 0 // a V3-family pool: it calls back for its input
	chainPair   = 1 // a V2 pair: it is paid before it swaps
	chainMetric = 2
)

// chainAmountBits is the width of a step's amount in its word.
const chainAmountBits = 88

func chainKind(kind domain.PoolKind) (uint8, bool) {
	switch kind {
	case domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindCamelotV3:
		return chainPool, true
	case domain.PoolKindV2:
		return chainPair, true
	case domain.PoolKindMetricOmm:
		return chainMetric, true
	}
	return 0, false
}

// HotChainPoolsSupported reports whether executeHot can run a route
// over these pools. Every swap is nested in the one after it and each pool
// pays the next directly, so: a pool appears once, because a pool is locked
// while it swaps; at least one pool calls back, the last of which (the
// outermost) is swapped for its exact input; and a Metric pool is only ever
// the outermost, because the pools before it are asked for an exact output.
func HotChainPoolsSupported(funding Funding, pools []domain.PoolKey) bool {
	if funding != FundingV3FlashSwap || len(pools) < 2 || len(pools) > 16 {
		return false
	}
	outermost := -1
	for i, key := range pools {
		kind, ok := chainKind(key.Kind)
		if !ok {
			return false
		}
		for _, earlier := range pools[:i] {
			if earlier.Address == key.Address {
				return false
			}
		}
		if kind != chainPair {
			outermost = i
		}
	}
	if outermost < 0 {
		return false
	}
	for _, key := range pools[:outermost] {
		if key.Kind == domain.PoolKindMetricOmm {
			return false
		}
	}
	return true
}

// HotChainSupported is HotChainPoolsSupported for a built plan: also its
// quoted amounts within the step word, no price limits and no cumulative check.
func HotChainSupported(plan ExecutorPlan) bool {
	if plan.MinRate != nil || plan.Amount == nil || plan.Amount.BitLen() > 128 || plan.MinProfit == nil || plan.MinProfit.BitLen() > 128 {
		return false
	}
	pools := make([]domain.PoolKey, len(plan.Steps))
	for i, step := range plan.Steps {
		if step.SqrtPriceLimitX96 != nil && step.SqrtPriceLimitX96.Sign() != 0 {
			return false
		}
		if step.AmountIn == nil || step.AmountOut == nil || step.AmountIn.BitLen() > chainAmountBits || step.AmountOut.BitLen() > chainAmountBits {
			return false
		}
		pools[i] = step.Pool
	}
	return HotChainPoolsSupported(plan.Funding, pools)
}

// HotChainCalldataBound is the largest executeHot calldata for steps hops:
// the 53-byte header, a 32-byte word per step and a price bound on every
// step, ABI-wrapped.
func HotChainCalldataBound(steps int) int {
	return 68 + (53+53*steps+31)/32*32
}

// EncodeHotChainPlan targets executeHot(bytes): the asset, 16-byte amount and
// minProfit, the step count, then one word per step: flags (bit 0 zeroForOne,
// bits 1-2 the kind), an 11-byte amount and the pool; then 21 bytes per price
// bound as EncodeHotV3Plan. A step's amount is its quoted input for the
// outermost pool and for a pair before it, its quoted output for a pair after
// it, and zero for a pool before the outermost, which is asked for what the
// next pool is owed. The contract checks no pause or deadline: the
// transaction's conditions carry them (HotConditions).
func EncodeHotChainPlan(ctx context.Context, plan ExecutorPlan) ([]byte, error) {
	if err := checkExecutorPlan(ctx, plan); err != nil {
		return nil, err
	}
	if !HotChainSupported(plan) {
		return nil, fmt.Errorf("the chained hot path cannot run this route")
	}
	outermost := 0
	for i, step := range plan.Steps {
		if step.Pool.Kind != domain.PoolKindV2 {
			outermost = i
		}
	}
	payload := make([]byte, 53, 53+len(plan.Steps)*53)
	copy(payload[:20], plan.Asset[:])
	plan.Amount.FillBytes(payload[20:36])
	plan.MinProfit.FillBytes(payload[36:52])
	payload[52] = byte(len(plan.Steps))
	if plan.Claim {
		payload[52] |= 0x80
	}
	for i, step := range plan.Steps {
		kind, _ := chainKind(step.Pool.Kind)
		word := make([]byte, 32)
		word[0] = kind << 1
		if bytes.Compare(step.TokenIn[:], step.TokenOut[:]) < 0 {
			word[0] |= 1
		}
		switch {
		case i == outermost, i < outermost && kind == chainPair:
			step.AmountIn.FillBytes(word[1:12])
		case i > outermost:
			step.AmountOut.FillBytes(word[1:12])
		}
		copy(word[12:], step.Pool.Address[:])
		payload = append(payload, word...)
	}
	for i, step := range plan.Steps {
		if step.PriceBound == nil {
			continue
		}
		if kind, _ := chainKind(step.Pool.Kind); kind != chainPool {
			return nil, fmt.Errorf("step %d: only a V3-family pool carries a price bound on the chained hot path", i)
		}
		index := byte(i)
		if step.Pool.Kind == domain.PoolKindCamelotV3 {
			index |= 0x80
		}
		payload = append(payload, index)
		payload = append(payload, make([]byte, 20)...)
		step.PriceBound.FillBytes(payload[len(payload)-20:])
	}
	return encodeBytesCall("executeHot(bytes)", payload), nil
}
