package exec

import (
	"context"
	"encoding/binary"
	"math/big"
)

// rateOne is 1.0 in the cumulative precheck's 1e18 fixed point.
var rateOne = big.NewInt(1e18)

// EncodePackedExecutorPlan targets executePacked(bytes) and matches
// contracts/src/PackedPlan.sol: a 95-byte header, an optional 53-byte V3 flash
// lender, then 80 bytes per step plus an optional 20-byte price limit. A plan
// with any price bound is version 3, which appends each bound after the
// step's limit; otherwise version 2, which every deployed executor decodes.
// A plan with MinRate is version 4: the 8-byte rate follows the header and
// the steps' prices are quote references for the cumulative check.
func EncodePackedExecutorPlan(ctx context.Context, plan ExecutorPlan) ([]byte, error) {
	// Boundary validation is shared with the ABI encoder. This runs off-chain.
	if err := checkExecutorPlan(ctx, plan); err != nil {
		return nil, err
	}
	version := byte(2)
	for _, step := range plan.Steps {
		if step.PriceBound != nil {
			version = 3
		}
	}
	if plan.MinRate != nil {
		version = 4
	}
	payload := make([]byte, 95, 156+len(plan.Steps)*120)
	payload[0], payload[1], payload[2] = version, byte(plan.Funding), byte(len(plan.Steps))
	copy(payload[3:23], plan.Asset[:])
	plan.Amount.FillBytes(payload[23:55])
	plan.MinProfit.FillBytes(payload[55:87])
	binary.BigEndian.PutUint64(payload[87:95], plan.Deadline)
	if version == 4 {
		payload = binary.BigEndian.AppendUint64(payload, plan.MinRate.Uint64())
	}
	if plan.Funding == FundingV3Flash {
		payload = append(payload, plan.Lender.Address[:]...)
		direction := byte(0)
		if plan.LenderToken0 {
			direction = 1
		}
		payload = append(payload, direction)
		payload = append(payload, make([]byte, 32)...)
		plan.MaxLoanFee.FillBytes(payload[len(payload)-32:])
	}
	for _, step := range plan.Steps {
		payload = append(payload, byte(step.Pool.Kind))
		payload = append(payload, step.Pool.Address[:]...)
		payload = append(payload, step.TokenOut[:]...)
		payload = append(payload, byte(step.Fee>>16), byte(step.Fee>>8), byte(step.Fee))
		spacing := uint32(step.TickSpacing)
		payload = append(payload, byte(spacing>>16), byte(spacing>>8), byte(spacing))
		payload = append(payload, make([]byte, 32)...)
		step.MinAmountOut.FillBytes(payload[len(payload)-32:])
		limited := step.SqrtPriceLimitX96 != nil && step.SqrtPriceLimitX96.Sign() != 0
		flags := byte(0)
		if limited {
			flags |= 1
		}
		if step.PriceBound != nil {
			flags |= 2
		}
		payload = append(payload, flags)
		if limited {
			payload = append(payload, make([]byte, 20)...)
			step.SqrtPriceLimitX96.FillBytes(payload[len(payload)-20:])
		}
		if step.PriceBound != nil {
			payload = append(payload, make([]byte, 20)...)
			step.PriceBound.FillBytes(payload[len(payload)-20:])
		}
	}
	return encodeBytesCall("executePacked(bytes)", payload), nil
}

// PackedCalldataBound is the largest executePacked calldata for a route of
// steps hops whose steps carry at most one optional 20-byte field: the 95-byte
// header, version 4's 8-byte rate, the 53-byte standalone flash extension and
// 100 bytes per step, ABI-wrapped. Planning sets price bounds but never limits.
func PackedCalldataBound(steps int) int {
	payload := 95 + 8 + 53 + 100*steps
	return 68 + (payload+31)/32*32
}
