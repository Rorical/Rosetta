package pipeline

import (
	"context"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
	poolstate "github.com/Rorical/Rosetta/internal/state"
)

func explainSwapDifference(ctx context.Context, row *SwapComparison, p pool.State, view poolstate.View, fees map[domain.PoolKey]pool.State) {
	deep, _ := view.Deep(p.Key)
	if matchesExactOutput(p, deep, row.TokenIn, row.Input, row.Observed) {
		row.Explanation = "exact_output_rounding_match"
		return
	}
	refreshed, ok := fees[p.Key]
	if !ok {
		row.Explanation = "unresolved_fee_intent_or_state"
		return
	}
	p.Fee, p.ProtocolFee, p.FeeOneForZero = refreshed.Fee, refreshed.ProtocolFee, refreshed.FeeOneForZero
	p.Slipstream = nil // Requote the independently observed fee, not the local model.
	fee, err := dex.SwapFee(p, row.TokenIn)
	if err != nil || fee == row.Fee {
		row.Explanation = "unresolved_fee_intent_or_state"
		return
	}
	row.RefreshedFee = &fee
	adapter, err := dex.NewAdapter(p)
	if err != nil {
		return
	}
	row.Requoted, err = adapter.QuoteExactIn(ctx, p, deep, row.TokenIn, row.Input)
	if err != nil {
		return
	}
	row.RequotedDelta = new(big.Int).Sub(row.Requoted, row.Observed)
	row.Explanation = "residual_after_fee_refresh"
	if row.RequotedDelta.Sign() == 0 {
		row.Explanation = "canonical_fee_refresh_match"
	} else if matchesExactOutput(p, deep, row.TokenIn, row.Input, row.Observed) {
		row.Explanation = "canonical_fee_and_exact_output_match"
	}
	// Post-block fee observations explain a past comparison. They must never
	// replace the historical pre-state or be treated as the executed swap's fee.
}
func matchesExactOutput(p pool.State, deep pool.DeepState, token domain.Address, input, output *big.Int) bool {
	switch p.Key.Kind {
	case domain.PoolKindV3, domain.PoolKindSlipstream, domain.PoolKindCamelotV3, domain.PoolKindSolidlyV3:
	default:
		return false
	}
	fee, err := dex.SwapFee(p, token)
	if err != nil {
		return false
	}
	result, err := math.SwapV3ExactOut(math.V3State{SqrtPriceX96: p.SqrtPriceX96, Tick: p.Tick, Liquidity: p.Liquidity, FeePips: fee, TickSpacing: p.TickSpacing, Bitmap: deep.Bitmap, Ticks: deep.Ticks, WindowMin: deep.WindowMin, WindowMax: deep.WindowMax}, token == p.Token0, output)
	return err == nil && result.AmountInUsed.Cmp(input) == 0
}
