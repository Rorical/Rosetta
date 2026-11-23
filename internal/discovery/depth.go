package discovery

import (
	"context"
	"fmt"
	"math/big"

	"github.com/Rorical/Rosetta/internal/dex"
	"github.com/Rorical/Rosetta/internal/pool"
)

// VerifyDepth checks a shortlist estimate against the actual adapter, including
// initialized ticks. Slippage is measured against the fee-adjusted spot quote.
func VerifyDepth(ctx context.Context, state pool.State, deep pool.DeepState, row Assessment, maxBPS uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if row.TargetIn == nil {
		return fmt.Errorf("market has no target amount")
	}
	adapter, err := dex.NewAdapter(state)
	if err != nil {
		return err
	}
	actual, err := adapter.QuoteExactIn(ctx, state, deep, row.TokenIn, row.TargetIn)
	if err != nil {
		return err
	}
	r0, r1, ok := marketSpotReserves(state)
	if !ok {
		return fmt.Errorf("market has no reserves")
	}
	in, out := r0, r1
	if row.TokenIn == state.Token1 {
		in, out = out, in
	}
	fee, err := dex.SwapFee(state, row.TokenIn)
	if err != nil {
		return err
	}
	ideal := new(big.Int).Div(new(big.Int).Mul(new(big.Int).Mul(row.TargetIn, out), new(big.Int).SetUint64(1000000-uint64(fee))), new(big.Int).Mul(in, big.NewInt(1000000)))
	if ideal.Sign() <= 0 || actual.Sign() <= 0 {
		return fmt.Errorf("market target quote rounds to zero")
	}
	loss := new(big.Int).Sub(ideal, actual)
	loss.Mul(loss, big.NewInt(10000))
	if loss.Cmp(new(big.Int).Mul(ideal, new(big.Int).SetUint64(maxBPS))) > 0 {
		return fmt.Errorf("market target exceeds slippage limit")
	}
	return nil
}
