package ingest

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestApplyV3SwapUpdatesState(t *testing.T) {
	addr := domain.Address{0x11}
	store := storeWith(t, v3Pool(addr))
	applyLogs(t, store, []domain.PoolKey{v3Key(addr)}, v3SwapLog(addr, 12_345, 777, -42))

	got, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("pool %s missing", addr)
	}
	assertInt(t, "sqrt price", got.SqrtPriceX96, 12_345)
	assertInt(t, "liquidity", got.Liquidity, 777)
	if got.Tick != -42 {
		t.Fatalf("tick = %d, want -42", got.Tick)
	}
	if got.UpdatedBlock != testBlockNumber {
		t.Fatalf("updated block = %d, want %d", got.UpdatedBlock, testBlockNumber)
	}
	if block := store.Block(); block.Number != testBlockNumber || block.Hash != testHash {
		t.Fatalf("block = %+v, want number %d hash %s", block, testBlockNumber, testHash)
	}
}

func TestApplyV3ShapedSwapUpdatesState(t *testing.T) {
	for _, kind := range []domain.PoolKind{domain.PoolKindV3, domain.PoolKindCamelotV3, domain.PoolKindSlipstream} {
		t.Run(fmt.Sprint(kind), func(t *testing.T) {
			addr := domain.Address{0x14}
			initial := v3Pool(addr)
			initial.Key.Kind = kind
			store := storeWith(t, initial)
			applyLogs(t, store, []domain.PoolKey{initial.Key}, v3SwapLog(addr, 12_345, 777, -42))

			got, ok := store.Pool(initial.Key)
			if !ok {
				t.Fatalf("pool %s missing", addr)
			}
			if got.Key.Kind != kind {
				t.Fatalf("kind = %d, want %d", got.Key.Kind, kind)
			}
			assertInt(t, "sqrt price", got.SqrtPriceX96, 12_345)
			assertInt(t, "liquidity", got.Liquidity, 777)
			if got.Tick != -42 || got.UpdatedBlock != testBlockNumber {
				t.Fatalf("tick/block = %d/%d, want -42/%d", got.Tick, got.UpdatedBlock, testBlockNumber)
			}
		})
	}
}

func TestApplyV3MintAddsLiquidity(t *testing.T) {
	addr := domain.Address{0x12}
	store := storeWith(t, v3Pool(addr))
	applyLogs(t, store, []domain.PoolKey{v3Key(addr)}, v3MintLog(addr, -60, 60, 1000))

	got, ok := store.Pool(v3Key(addr))
	if !ok {
		t.Fatalf("pool %s missing", addr)
	}
	assertInt(t, "liquidity", got.Liquidity, 1000)
	deep, ok := store.Deep(v3Key(addr))
	if !ok {
		t.Fatalf("deep state for %s missing", addr)
	}
	assertTick(t, deep, -60, 1000, 1000)
	assertTick(t, deep, 60, 1000, -1000)
}

// A ModifyLiquidity log for one of two pool IDs behind the same PoolManager
// advances only that pool's shallow and deep state.
func TestApplyV4ModifyLiquidityUpdatesOneSibling(t *testing.T) {
	manager := domain.Address{0x50}
	firstID, secondID := domain.Hash{0x01}, domain.Hash{0x02}
	first, second := v4Pool(manager, firstID), v4Pool(manager, secondID)
	store := storeWith(t, first, second)
	dirty := applyLogs(t, store, []domain.PoolKey{first.Key, second.Key}, v4ModifyLiquidityLog(manager, firstID, -120, 120, 500))

	updated, ok := store.Pool(first.Key)
	if !ok {
		t.Fatalf("pool %s missing", firstID.Hex())
	}
	assertInt(t, "liquidity", updated.Liquidity, 500)
	if updated.UpdatedBlock != testBlockNumber {
		t.Fatalf("updated block = %d, want %d", updated.UpdatedBlock, testBlockNumber)
	}
	deep, ok := store.Deep(first.Key)
	if !ok {
		t.Fatalf("deep state for %s missing", firstID.Hex())
	}
	assertTick(t, deep, -120, 500, 500)
	assertTick(t, deep, 120, 500, -500)

	untouched, ok := store.Pool(second.Key)
	if !ok {
		t.Fatalf("sibling pool %s missing", secondID.Hex())
	}
	assertInt(t, "sibling liquidity", untouched.Liquidity, 0)
	if untouched.UpdatedBlock != 0 {
		t.Fatalf("sibling updated block = %d, want 0", untouched.UpdatedBlock)
	}
	if _, ok := store.Deep(second.Key); ok {
		t.Fatalf("sibling %s gained deep state", secondID.Hex())
	}
	if !reflect.DeepEqual(dirty, []domain.PoolKey{first.Key}) {
		t.Fatalf("dirty = %v, want only %s", dirty, firstID.Hex())
	}
}

func TestApplyV4SwapUpdatesState(t *testing.T) {
	manager := domain.Address{0x51}
	poolID := domain.Hash{0x03}
	initial := v4Pool(manager, poolID)
	store := storeWith(t, initial)
	applyLogs(t, store, []domain.PoolKey{initial.Key}, v4SwapLog(manager, poolID, 12_345, 777, -42))

	got, ok := store.Pool(initial.Key)
	if !ok {
		t.Fatalf("pool %s missing", poolID.Hex())
	}
	assertInt(t, "sqrt price", got.SqrtPriceX96, 12_345)
	assertInt(t, "liquidity", got.Liquidity, 777)
	if got.Tick != -42 || got.UpdatedBlock != testBlockNumber {
		t.Fatalf("tick/block = %d/%d, want -42/%d", got.Tick, got.UpdatedBlock, testBlockNumber)
	}
}

func TestApplyV2SyncUpdatesReserves(t *testing.T) {
	addr := domain.Address{0x21}
	store := storeWith(t, v2Pool(addr))
	applyLogs(t, store, []domain.PoolKey{v2Key(addr)}, v2SyncLog(addr, 500, 700))

	got, ok := store.Pool(v2Key(addr))
	if !ok {
		t.Fatalf("pool %s missing", addr)
	}
	assertInt(t, "reserve0", got.Reserve0, 500)
	assertInt(t, "reserve1", got.Reserve1, 700)
	if got.UpdatedBlock != testBlockNumber {
		t.Fatalf("updated block = %d, want %d", got.UpdatedBlock, testBlockNumber)
	}
	if got.Key != v2Key(addr) {
		t.Fatalf("pool key = %+v, want preserved identity", got.Key)
	}
}

func TestApplyIgnoresUnknownPool(t *testing.T) {
	tracked, other := domain.Address{0x16}, domain.Address{0x17}
	store := storeWith(t, v3Pool(tracked))
	dirty := applyLogs(t, store, []domain.PoolKey{v3Key(tracked)}, v3SwapLog(other, 12_345, 777, -42))

	if _, ok := store.Pool(v3Key(other)); ok {
		t.Fatalf("untracked pool %s was written", other)
	}
	if len(dirty) != 0 {
		t.Fatalf("dirty pools = %v, want none", dirty)
	}
}

// Two pool IDs behind one PoolManager resolve to their own keys; an untracked
// pool ID or an untracked address resolves to nothing.
func TestRoutingResolvesV4Siblings(t *testing.T) {
	manager := domain.Address{0x50}
	first := domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{0x01}}
	second := domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{0x02}, Hooks: domain.Address{0xcc}}
	routes, err := newRouting([]domain.PoolKey{first, second})
	if err != nil {
		t.Fatalf("newRouting: %v", err)
	}
	for _, test := range []struct {
		name string
		log  Log
		want domain.PoolKey
		ok   bool
	}{
		{"first pool id", Log{Address: manager, Topics: []domain.Hash{{0xdd}, first.PoolID}}, first, true},
		{"second pool id", Log{Address: manager, Topics: []domain.Hash{{0xdd}, second.PoolID}}, second, true},
		{"untracked pool id", Log{Address: manager, Topics: []domain.Hash{{0xdd}, {0x03}}}, domain.PoolKey{}, false},
		{"manager without topics", Log{Address: manager}, domain.PoolKey{}, false},
		{"untracked address", Log{Address: domain.Address{0x53}, Topics: []domain.Hash{{0xdd}, first.PoolID}}, domain.PoolKey{}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := routes.resolveKey(test.log)
			if ok != test.ok || got != test.want {
				t.Fatalf("resolveKey() = (%+v, %v), want (%+v, %v)", got, ok, test.want, test.ok)
			}
		})
	}
}

// One transaction holding a tracked V3 swap, two V4 logs with an unrecognized
// topic at tracked pool IDs, and a V3-shaped log at an untracked address
// changes only the tracked V3 pool; an unknown V4 event is skipped.
func TestApplySkipsUntrackedAndUnknownV4Logs(t *testing.T) {
	tracked, untracked, manager := domain.Address{0x51}, domain.Address{0x53}, domain.Address{0x52}
	first := domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{0x01}}
	second := domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: domain.Hash{0x02}}
	store := storeWith(t, v3Pool(tracked))
	applyLogs(t, store, []domain.PoolKey{v3Key(tracked), first, second},
		v3SwapLog(tracked, 12_345, 777, -42),
		Log{Address: manager, Topics: []domain.Hash{{0xdd}, first.PoolID}},
		Log{Address: manager, Topics: []domain.Hash{{0xdd}, second.PoolID}},
		v3SwapLog(untracked, 1, 2, 3),
	)

	got, ok := store.Pool(v3Key(tracked))
	if !ok {
		t.Fatalf("tracked pool %s missing", tracked)
	}
	assertInt(t, "sqrt price", got.SqrtPriceX96, 12_345)
	if len(store.Pools()) != 1 {
		t.Fatalf("pools = %d, want only the tracked V3 pool", len(store.Pools()))
	}
}

// The wrap pool's address is the wrapped native token, which logs every
// transfer of the token and nothing a pool state follows: its logs are not
// ours, and it does not collide with pools routed by address.
func TestRoutingLeavesTheWrapPoolOut(t *testing.T) {
	wrapped := domain.Address{0x82}
	wrap := domain.PoolKey{Address: wrapped, Kind: domain.PoolKindWrap}
	pair := domain.PoolKey{Address: domain.Address{0x51}, Kind: domain.PoolKindV2}
	routes, err := newRouting([]domain.PoolKey{wrap, pair})
	if err != nil {
		t.Fatalf("newRouting: %v", err)
	}
	if key, ok := routes.resolveKey(Log{Address: wrapped, Topics: []domain.Hash{{0xdd}}}); ok {
		t.Fatalf("a log of the wrapped token resolved to %+v", key)
	}
	if key, ok := routes.resolveKey(Log{Address: pair.Address}); !ok || key != pair {
		t.Fatalf("pair log resolved to (%+v, %v)", key, ok)
	}
}
