package ingest

import (
	"context"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
	"github.com/Rorical/Rosetta/internal/state"
)

const testBlockNumber = 1000

var testHash = domain.Hash{0x42}

// applyLogs routes logs to keys and applies them as one successful
// transaction in testRef's block, returning the pools it changed.
func applyLogs(t *testing.T, store *state.Store, keys []domain.PoolKey, logs ...Log) []domain.PoolKey {
	t.Helper()
	routes, err := newRouting(keys)
	if err != nil {
		t.Fatalf("newRouting: %v", err)
	}
	dirty, err := applyTransaction(context.Background(), store, routes, testRef(), Transaction{Hash: domain.Hash{0x01}, Successful: true, Logs: logs})
	if err != nil {
		t.Fatalf("applyTransaction: %v", err)
	}
	return dirty
}

// committedAt moves a store to ref and closes the frame so later blocks start
// from a committed head.
func committedAt(t *testing.T, store *state.Store, ref domain.BlockRef) {
	t.Helper()
	if err := store.BeginBlock(ref); err != nil {
		t.Fatalf("BeginBlock(%d): %v", ref.Number, err)
	}
	store.Commit()
}

func testRef() domain.BlockRef {
	return domain.BlockRef{Number: testBlockNumber, Hash: testHash, Timestamp: 1_700_000_000, L1Block: 900}
}

func storeWith(t *testing.T, states ...pool.State) *state.Store {
	t.Helper()
	store := state.New()
	for _, state := range states {
		if err := store.Upsert(state); err != nil {
			t.Fatalf("Upsert: %v", err)
		}
	}
	store.ClearDirty()
	return store
}

// v3Key is the key of the V3 pool v3Pool builds at addr.
func v3Key(addr domain.Address) domain.PoolKey {
	return domain.PoolKey{Address: addr, Kind: domain.PoolKindV3}
}

// v2Key is the key of the V2 pool v2Pool builds at addr.
func v2Key(addr domain.Address) domain.PoolKey {
	return domain.PoolKey{Address: addr, Kind: domain.PoolKindV2}
}

func v3Pool(addr domain.Address) pool.State {
	return pool.State{
		Key:          domain.PoolKey{Address: addr, Kind: domain.PoolKindV3},
		Token0:       domain.Address{0xaa},
		Token1:       domain.Address{0xbb},
		Fee:          3000,
		TickSpacing:  60,
		SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96),
		Tick:         0,
		Liquidity:    new(big.Int),
	}
}

// seededV3Pool returns a store holding one initialized V3 pool with a 500
// liquidity position spanning -120..120 and an empty dirty set.
func seededV3Pool(t *testing.T, addr domain.Address) *state.Store {
	t.Helper()
	store := storeWith(t, v3Pool(addr))
	initialize := uniswapv3.Event{
		Kind:         uniswapv3.EventInitialize,
		SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96),
		Tick:         0,
	}
	if err := store.ApplyV3(v3Key(addr), initialize, 0); err != nil {
		t.Fatalf("ApplyV3 initialize: %v", err)
	}
	mint := uniswapv3.Event{
		Kind:      uniswapv3.EventMint,
		TickLower: -120,
		TickUpper: 120,
		Amount:    big.NewInt(500),
	}
	if err := store.ApplyV3(v3Key(addr), mint, 0); err != nil {
		t.Fatalf("ApplyV3 mint: %v", err)
	}
	store.ClearDirty()
	return store
}

func v2Pool(addr domain.Address) pool.State {
	return pool.State{
		Key:      domain.PoolKey{Address: addr, Kind: domain.PoolKindV2},
		Token0:   domain.Address{0xaa},
		Token1:   domain.Address{0xbb},
		Fee:      3000,
		Reserve0: big.NewInt(1000),
		Reserve1: big.NewInt(2000),
	}
}

func v3MintLog(addr domain.Address, lower, upper int32, amount int64) Log {
	return Log{
		Address: addr,
		Topics:  []domain.Hash{uniswapv3.TopicMint, topicAddress(0x01), topicInt24(lower), topicInt24(upper)},
		// Topic0 Mint data is sender, amount, amount0, amount1.
		Data: encodeWords(big.NewInt(0xbeef), big.NewInt(amount), big.NewInt(0), big.NewInt(0)),
	}
}

func v3SwapLog(addr domain.Address, sqrtPrice, liquidity int64, tick int32) Log {
	return Log{
		Address: addr,
		Topics:  []domain.Hash{uniswapv3.TopicSwap, topicAddress(0x01), topicAddress(0x02)},
		Data: encodeWords(
			big.NewInt(-1),
			big.NewInt(1),
			big.NewInt(sqrtPrice),
			big.NewInt(liquidity),
			big.NewInt(int64(tick)),
		),
	}
}

// V4 PoolManager event selectors.
var (
	v4SwapTopic            = domain.Hash{0x40, 0xe9, 0xce, 0xcb}
	v4ModifyLiquidityTopic = domain.Hash{0xf2, 0x08, 0xf4, 0x91}
)

func v4Key(manager domain.Address, poolID domain.Hash) domain.PoolKey {
	return domain.PoolKey{Address: manager, Kind: domain.PoolKindV4, PoolID: poolID}
}

func v4Pool(manager domain.Address, poolID domain.Hash) pool.State {
	return pool.State{
		Key:          v4Key(manager, poolID),
		Token0:       domain.Address{0xaa},
		Token1:       domain.Address{0xbb},
		Fee:          500,
		ProtocolFee:  0x07d07d,
		TickSpacing:  10,
		SqrtPriceX96: new(big.Int).Lsh(big.NewInt(1), 96),
		Tick:         0,
		Liquidity:    new(big.Int),
	}
}

// v4ModifyLiquidityLog is a PoolManager ModifyLiquidity log: topic1 is the
// pool ID and topic2 the sender; the tick bounds and delta are in the data.
func v4ModifyLiquidityLog(manager domain.Address, poolID domain.Hash, lower, upper int32, delta int64) Log {
	return Log{
		Address: manager,
		Topics:  []domain.Hash{v4ModifyLiquidityTopic, poolID, topicAddress(0x0d)},
		Data: encodeWords(
			big.NewInt(int64(lower)),
			big.NewInt(int64(upper)),
			big.NewInt(delta),
			big.NewInt(0),
		),
	}
}

func v4SwapLog(manager domain.Address, poolID domain.Hash, sqrtPrice, liquidity int64, tick int32) Log {
	return Log{
		Address: manager,
		Topics:  []domain.Hash{v4SwapTopic, poolID, topicAddress(0x0e)},
		Data: encodeWords(
			big.NewInt(0),
			big.NewInt(0),
			big.NewInt(sqrtPrice),
			big.NewInt(liquidity),
			big.NewInt(int64(tick)),
			big.NewInt(625),
		),
	}
}

func v2SyncLog(addr domain.Address, reserve0, reserve1 int64) Log {
	return Log{
		Address: addr,
		Topics:  []domain.Hash{uniswapv2.TopicSync},
		Data:    encodeWords(big.NewInt(reserve0), big.NewInt(reserve1)),
	}
}

func encodeWords(values ...*big.Int) []byte {
	out := make([]byte, 0, len(values)*32)
	for _, value := range values {
		encoded := new(big.Int).Set(value)
		if encoded.Sign() < 0 {
			encoded.Add(encoded, new(big.Int).Lsh(big.NewInt(1), 256))
		}
		word := make([]byte, 32)
		encoded.FillBytes(word)
		out = append(out, word...)
	}
	return out
}

func topicInt24(tick int32) domain.Hash {
	var hash domain.Hash
	copy(hash[:], encodeWords(big.NewInt(int64(tick))))
	return hash
}

func topicAddress(b byte) domain.Hash {
	var hash domain.Hash
	hash[31] = b
	return hash
}

func assertInt(t *testing.T, name string, got *big.Int, want int64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s is nil, want %d", name, want)
	}
	if got.Cmp(big.NewInt(want)) != 0 {
		t.Fatalf("%s = %s, want %d", name, got, want)
	}
}

func assertTick(t *testing.T, deep pool.DeepState, tick int32, gross, net int64) {
	t.Helper()
	info, ok := deep.Ticks[tick]
	if !ok {
		t.Fatalf("tick %d missing", tick)
	}
	assertInt(t, "tick gross", info.LiquidityGross, gross)
	assertInt(t, "tick net", info.LiquidityNet, net)
}
