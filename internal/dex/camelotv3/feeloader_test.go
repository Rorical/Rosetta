package camelotv3

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestLoadFeePreservesReceiptStateAndAcceptsZero(t *testing.T) {
	for _, fee := range []int64{0, 500} {
		caller := newFakeCaller()
		caller.setOut(globalStateCall, globalStateResult(q96(), 999, big.NewInt(fee), 0))
		original := pool.State{Key: domain.PoolKey{Kind: domain.PoolKindCamelotV3, Address: domain.Address{1}}, Token0: domain.Address{2}, Token1: domain.Address{3},
			Fee: 3000, TickSpacing: 10, SqrtPriceX96: q96(), Tick: 1, Liquidity: big.NewInt(123), UpdatedBlock: 7}
		updated, err := LoadFee(t.Context(), caller, original, "0x7")
		if err != nil || updated.Fee != uint32(fee) || updated.Tick != original.Tick || updated.Liquidity.Cmp(original.Liquidity) != 0 || updated.UpdatedBlock != original.UpdatedBlock {
			t.Fatalf("fee refresh replaced receipt state: %+v %v", updated, err)
		}
		updated.Liquidity.SetInt64(999)
		if original.Fee != 3000 || original.Liquidity.Int64() != 123 || len(caller.calls) != 1 {
			t.Fatal("fee-only refresh aliased its input or read unrelated state")
		}
	}
}
