package exec

import (
	"context"
	"fmt"

	"github.com/Rorical/Rosetta/internal/domain"
)

// HotRouteSupported reports whether executeV3Hot (contracts/src/HotV3Executor.sol)
// can run plan: V3 flash-swap funding, every leg on a pool whose callback
// the hot path accepts (Uniswap, Sushi and Pancake V3, Slipstream, Camelot V3),
// amounts within 128 bits, and neither price limits nor the cumulative check.
func HotRouteSupported(plan ExecutorPlan) bool {
	if plan.MinRate != nil || plan.Amount == nil || plan.Amount.BitLen() > 128 || plan.MinProfit == nil || plan.MinProfit.BitLen() > 128 {
		return false
	}
	pools := make([]domain.PoolKey, len(plan.Steps))
	for i, step := range plan.Steps {
		if step.SqrtPriceLimitX96 != nil && step.SqrtPriceLimitX96.Sign() != 0 {
			return false
		}
		pools[i] = step.Pool
	}
	return HotPoolsSupported(plan.Funding, pools)
}

// HotPoolsSupported is HotRouteSupported before a plan exists: the funding
// and the pools' kinds.
func HotPoolsSupported(funding Funding, pools []domain.PoolKey) bool {
	if funding != FundingV3FlashSwap || len(pools) < 2 || len(pools) > 16 {
		return false
	}
	for _, key := range pools {
		switch key.Kind {
		case domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindCamelotV3:
		default:
			return false
		}
	}
	return true
}

// HotCalldataBound is the largest executeV3Hot calldata for steps hops: the
// 53-byte header and 40 bytes per step, a price bound on every step, ABI-wrapped.
func HotCalldataBound(steps int) int {
	return 68 + (53+61*steps+31)/32*32
}

// EncodeHotV3Plan targets executeV3Hot(bytes): the asset, 16-byte amount and
// minProfit, the step count, 40 bytes per step (pool, tokenOut), then 21 bytes
// per price bound (the step index, bit 7 set to read Algebra's globalState,
// and the 20-byte sqrt price). The contract checks no pause, deadline or
// per-leg minimum: the transaction's conditions carry the pause and the
// plan's Deadline (HotConditions), and the last leg's floor and the actual
// profit bound the route.
func EncodeHotV3Plan(ctx context.Context, plan ExecutorPlan) ([]byte, error) {
	if err := checkExecutorPlan(ctx, plan); err != nil {
		return nil, err
	}
	if !HotRouteSupported(plan) {
		return nil, fmt.Errorf("the hot path runs only V3-family flash-swap routes")
	}
	payload := make([]byte, 53, 53+len(plan.Steps)*61)
	copy(payload[:20], plan.Asset[:])
	plan.Amount.FillBytes(payload[20:36])
	plan.MinProfit.FillBytes(payload[36:52])
	payload[52] = byte(len(plan.Steps))
	if plan.Claim {
		payload[52] |= 0x80
	}
	for _, step := range plan.Steps {
		payload = append(payload, step.Pool.Address[:]...)
		payload = append(payload, step.TokenOut[:]...)
	}
	for i, step := range plan.Steps {
		if step.PriceBound == nil {
			continue
		}
		index := byte(i)
		if step.Pool.Kind == domain.PoolKindCamelotV3 {
			index |= 0x80
		}
		payload = append(payload, index)
		payload = append(payload, make([]byte, 20)...)
		step.PriceBound.FillBytes(payload[len(payload)-20:])
	}
	return encodeBytesCall("executeV3Hot(bytes)", payload), nil
}

// encodeBytesCall ABI-encodes a call to a function taking one bytes argument.
func encodeBytesCall(signature string, payload []byte) []byte {
	selector := keccak([]byte(signature))
	encoded := make([]byte, 68+(len(payload)+31)/32*32)
	copy(encoded[:4], selector[:4])
	putUint64(encoded[4:36], 32)
	putUint64(encoded[36:68], uint64(len(payload)))
	copy(encoded[68:], payload)
	return encoded
}
