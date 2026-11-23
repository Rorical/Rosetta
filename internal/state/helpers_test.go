package state

import (
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func testAddress(b byte) domain.Address {
	return domain.Address{b}
}

func testQ96() *big.Int {
	return new(big.Int).Lsh(big.NewInt(1), 96)
}

// v3Key is the store key of the V3 pool v3State builds at addr.
func v3Key(addr domain.Address) domain.PoolKey {
	return domain.PoolKey{Address: addr, Kind: domain.PoolKindV3}
}

// v2Key is the store key of the V2 pool v2State builds at addr.
func v2Key(addr domain.Address) domain.PoolKey {
	return domain.PoolKey{Address: addr, Kind: domain.PoolKindV2}
}

func v3State(addr domain.Address) pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: addr,
			Kind:    domain.PoolKindV3,
		},
		Token0:       domain.Address{0xaa},
		Token1:       domain.Address{0xbb},
		Fee:          3000,
		TickSpacing:  10,
		SqrtPriceX96: testQ96(),
		Tick:         0,
		Liquidity:    big.NewInt(1_000_000),
	}
}

func v2State(addr domain.Address) pool.State {
	return pool.State{
		Key: domain.PoolKey{
			Address: addr,
			Kind:    domain.PoolKindV2,
		},
		Token0:   domain.Address{0xaa},
		Token1:   domain.Address{0xbb},
		Fee:      3000,
		Reserve0: big.NewInt(1_000_000),
		Reserve1: big.NewInt(2_000_000),
	}
}

// seededStore returns a store holding one initialized V3 pool with a minted
// position at a fixed nonzero head, deep state stamped with that head, and an
// empty dirty set. The nonzero head matters for rollbacks: deep state with no
// load block is treated as unknown and dropped.
func seededStore(t *testing.T, addr domain.Address) *Store {
	t.Helper()
	store := New()
	head := blockRef(10)
	if err := store.BeginBlock(head); err != nil {
		t.Fatalf("BeginBlock: %v", err)
	}
	if err := store.Upsert(v3State(addr)); err != nil {
		t.Fatalf("Upsert: %v", err)
	}
	if err := store.ApplyV3(v3Key(addr), initializeEvent(), store.Block().Number); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	if err := store.ApplyV3(v3Key(addr), mintEvent(-120, 120, 1_000_000), store.Block().Number); err != nil {
		t.Fatalf("ApplyV3 mint: %v", err)
	}
	store.Commit()
	deep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("Deep(%s) missing after seeding", addr)
	}
	deep.LoadedBlock = head
	if err := store.SetDeep(v3Key(addr), deep); err != nil {
		t.Fatalf("SetDeep: %v", err)
	}
	store.ClearDirty()
	return store
}

func initializeEvent() uniswapv3.Event {
	return uniswapv3.Event{Kind: uniswapv3.EventInitialize, SqrtPriceX96: testQ96(), Tick: 0}
}

func mintEvent(lower, upper int32, amount int64) uniswapv3.Event {
	return uniswapv3.Event{
		Kind:      uniswapv3.EventMint,
		TickLower: lower,
		TickUpper: upper,
		Amount:    big.NewInt(amount),
	}
}

func burnEvent(lower, upper int32, amount int64) uniswapv3.Event {
	return uniswapv3.Event{
		Kind:      uniswapv3.EventBurn,
		TickLower: lower,
		TickUpper: upper,
		Amount:    big.NewInt(amount),
	}
}

func swapEvent(sqrtPrice *big.Int, tick int32, liquidity int64) uniswapv3.Event {
	return uniswapv3.Event{
		Kind:         uniswapv3.EventSwap,
		SqrtPriceX96: sqrtPrice,
		Tick:         tick,
		Liquidity:    big.NewInt(liquidity),
	}
}

func assertPoolEqual(t *testing.T, got, want pool.State) {
	t.Helper()
	if got.Key != want.Key || got.Token0 != want.Token0 || got.Token1 != want.Token1 {
		t.Fatalf("pool identity = %+v, want %+v", got.Key, want.Key)
	}
	if got.Fee != want.Fee || got.ProtocolFee != want.ProtocolFee ||
		got.TickSpacing != want.TickSpacing || got.Tick != want.Tick || got.UpdatedBlock != want.UpdatedBlock {
		t.Fatalf("pool scalars = fee %d protocol fee %d spacing %d tick %d block %d, want fee %d protocol fee %d spacing %d tick %d block %d",
			got.Fee, got.ProtocolFee, got.TickSpacing, got.Tick, got.UpdatedBlock,
			want.Fee, want.ProtocolFee, want.TickSpacing, want.Tick, want.UpdatedBlock)
	}
	assertIntEqual(t, "sqrt price", got.SqrtPriceX96, want.SqrtPriceX96)
	assertIntEqual(t, "liquidity", got.Liquidity, want.Liquidity)
	assertIntEqual(t, "reserve0", got.Reserve0, want.Reserve0)
	assertIntEqual(t, "reserve1", got.Reserve1, want.Reserve1)
}

func assertDeepEqual(t *testing.T, got, want pool.DeepState) {
	t.Helper()
	if got.LoadedBlock != want.LoadedBlock {
		t.Fatalf("loaded block = %+v, want %+v", got.LoadedBlock, want.LoadedBlock)
	}
	if got.WindowMin != want.WindowMin || got.WindowMax != want.WindowMax {
		t.Fatalf("window = [%d, %d], want [%d, %d]", got.WindowMin, got.WindowMax, want.WindowMin, want.WindowMax)
	}
	if len(got.Bitmap) != len(want.Bitmap) {
		t.Fatalf("bitmap words = %d, want %d", len(got.Bitmap), len(want.Bitmap))
	}
	for position, word := range want.Bitmap {
		gotWord, ok := got.Bitmap[position]
		if !ok {
			t.Fatalf("bitmap word %d missing", position)
		}
		assertIntEqual(t, "bitmap word", gotWord, word)
	}
	if len(got.Ticks) != len(want.Ticks) {
		t.Fatalf("ticks = %d, want %d", len(got.Ticks), len(want.Ticks))
	}
	for tick, info := range want.Ticks {
		gotInfo, ok := got.Ticks[tick]
		if !ok {
			t.Fatalf("tick %d missing", tick)
		}
		assertIntEqual(t, "tick gross", gotInfo.LiquidityGross, info.LiquidityGross)
		assertIntEqual(t, "tick net", gotInfo.LiquidityNet, info.LiquidityNet)
	}
}

func assertIntEqual(t *testing.T, name string, got, want *big.Int) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("%s: got %v, want %v", name, got, want)
		}
		return
	}
	if got.Cmp(want) != 0 {
		t.Fatalf("%s = %s, want %s", name, got, want)
	}
}

func assertPoolKeys(t *testing.T, got, want []domain.PoolKey) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("pool keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("pool key %d = %s, want %s", i, formatPoolKey(got[i]), formatPoolKey(want[i]))
		}
	}
}
