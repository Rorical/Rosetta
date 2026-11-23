package pool

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestStateCopyIsDeep(t *testing.T) {
	q96 := new(big.Int).Lsh(big.NewInt(1), 96)
	original := State{
		Key: domain.PoolKey{
			Address: domain.Address{1},
			Kind:    domain.PoolKindV3,
		},
		Token0:       domain.Address{2},
		Token1:       domain.Address{3},
		Fee:          3000,
		ProtocolFee:  0x07d07d,
		TickSpacing:  10,
		SqrtPriceX96: q96,
		Tick:         -12,
		Liquidity:    big.NewInt(1_000_000),
		Reserve0:     big.NewInt(2_000_000),
		Reserve1:     big.NewInt(3_000_000),
		UpdatedBlock: 12345,
	}
	copied := original.Copy()

	if copied.Key != original.Key || copied.Token0 != original.Token0 || copied.Token1 != original.Token1 {
		t.Fatalf("copy changed identity fields")
	}
	if copied.Fee != original.Fee || copied.ProtocolFee != original.ProtocolFee ||
		copied.TickSpacing != original.TickSpacing || copied.Tick != original.Tick || copied.UpdatedBlock != original.UpdatedBlock {
		t.Fatalf("copy changed scalar fields")
	}
	assertDifferentInt(t, copied.SqrtPriceX96, original.SqrtPriceX96)
	assertDifferentInt(t, copied.Liquidity, original.Liquidity)
	assertDifferentInt(t, copied.Reserve0, original.Reserve0)
	assertDifferentInt(t, copied.Reserve1, original.Reserve1)

	copied.SqrtPriceX96.Add(copied.SqrtPriceX96, big.NewInt(1))
	copied.Liquidity.SetInt64(7)
	copied.Reserve0.Neg(copied.Reserve0)
	copied.Reserve1.SetInt64(0)
	copied.Tick = 99
	copied.Key.Kind = domain.PoolKindV2

	if original.SqrtPriceX96.Cmp(q96) != 0 {
		t.Fatalf("original sqrt price mutated: %s", original.SqrtPriceX96)
	}
	if original.Liquidity.Int64() != 1_000_000 || original.Reserve0.Int64() != 2_000_000 || original.Reserve1.Int64() != 3_000_000 {
		t.Fatalf("original reserves mutated")
	}
	if original.Tick != -12 || original.Key.Kind != domain.PoolKindV3 {
		t.Fatalf("original scalars mutated")
	}
}

func TestStateCopyPreservesNil(t *testing.T) {
	copied := (State{}).Copy()
	if copied.SqrtPriceX96 != nil || copied.Liquidity != nil || copied.Reserve0 != nil || copied.Reserve1 != nil {
		t.Fatalf("copy of zero state has non-nil big.Ints")
	}
}

func TestDeepStateCopyIsDeep(t *testing.T) {
	original := DeepState{
		Bitmap: map[int16]*big.Int{
			-1: new(big.Int).SetBit(new(big.Int), 255, 1),
		},
		Ticks: map[int32]TickInfo{
			-10: {
				LiquidityGross: big.NewInt(1_500_000),
				LiquidityNet:   big.NewInt(500_000),
			},
		},
		LoadedBlock: domain.BlockRef{Number: 42, Hash: domain.Hash{0x2a}, Timestamp: 100, L1Block: 7},
	}
	copied := original.Copy()

	if copied.LoadedBlock != original.LoadedBlock {
		t.Fatalf("copy changed loaded block: %+v, want %+v", copied.LoadedBlock, original.LoadedBlock)
	}
	if len(copied.Bitmap) != 1 || len(copied.Ticks) != 1 {
		t.Fatalf("copy has wrong sizes")
	}
	assertDifferentInt(t, copied.Bitmap[-1], original.Bitmap[-1])
	assertDifferentInt(t, copied.Ticks[-10].LiquidityGross, original.Ticks[-10].LiquidityGross)
	assertDifferentInt(t, copied.Ticks[-10].LiquidityNet, original.Ticks[-10].LiquidityNet)

	copied.Bitmap[-1].SetInt64(0)
	copied.Ticks[-10].LiquidityGross.SetInt64(0)
	copied.Ticks[-10].LiquidityNet.SetInt64(0)
	copied.Bitmap[5] = big.NewInt(1)
	copied.LoadedBlock.Number = 99
	delete(copied.Ticks, -10)

	if original.LoadedBlock.Number != 42 {
		t.Fatalf("original loaded block mutated: %+v", original.LoadedBlock)
	}
	if original.Bitmap[-1].BitLen() != 256 {
		t.Fatalf("original bitmap word mutated: %s", original.Bitmap[-1])
	}
	if original.Ticks[-10].LiquidityGross.Int64() != 1_500_000 || original.Ticks[-10].LiquidityNet.Int64() != 500_000 {
		t.Fatalf("original tick mutated")
	}
	if len(original.Bitmap) != 1 || len(original.Ticks) != 1 {
		t.Fatalf("original maps mutated")
	}
}

func TestDeepStateCopyPreservesNil(t *testing.T) {
	copied := (DeepState{}).Copy()
	if copied.Bitmap != nil || copied.Ticks != nil {
		t.Fatalf("copy of zero deep state has non-nil maps")
	}
}

func assertDifferentInt(t *testing.T, got, original *big.Int) {
	t.Helper()
	if got == nil || original == nil {
		t.Fatalf("unexpected nil big.Int")
	}
	if got == original {
		t.Fatalf("copy shares a big.Int pointer")
	}
	if got.Cmp(original) != 0 {
		t.Fatalf("copy value %s differs from original %s", got, original)
	}
}
