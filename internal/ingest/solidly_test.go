package ingest

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/solidlyv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

func TestSolidlySetFeeUpdatesFeeAndKeepsTicks(t *testing.T) {
	key := domain.PoolKey{Address: domain.Address{0x2b}, Kind: domain.PoolKindSolidlyV3}
	store := state.New()
	current := pool.State{Key: key, Token0: domain.Address{1}, Token1: domain.Address{2}, Fee: 100, TickSpacing: 1, SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96), Liquidity: big.NewInt(1000)}
	if err := store.Upsert(current); err != nil {
		t.Fatal(err)
	}
	deep := pool.DeepState{Bitmap: map[int16]*big.Int{0: big.NewInt(1)}, Ticks: map[int32]pool.TickInfo{0: {LiquidityGross: big.NewInt(1), LiquidityNet: big.NewInt(1)}}, WindowMin: -256, WindowMax: 255}
	if err := store.SetDeep(key, deep); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 64)
	data[31], data[63] = 100, 10
	if err := applyLog(store, Log{Address: key.Address, Topics: []domain.Hash{solidlyv3.TopicSetFee}, Data: data}, key, 7); err != nil {
		t.Fatal(err)
	}
	updated, _ := store.Pool(key)
	if updated.Fee != 10 || updated.UpdatedBlock != 7 {
		t.Fatal("SetFee not applied", updated.Fee, updated.UpdatedBlock)
	}
	if _, ok := store.View().Deep(key); !ok {
		t.Fatal("fee change dropped the tick window")
	}
}
