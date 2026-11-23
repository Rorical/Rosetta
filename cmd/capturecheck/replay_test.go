package main

import (
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/rpc"
)

func abiWord(value int64) []byte {
	out := make([]byte, 32)
	big.NewInt(value).FillBytes(out)
	return out
}

func hashWord(value int64) domain.Hash {
	var hash domain.Hash
	big.NewInt(value).FillBytes(hash[:])
	return hash
}

// swapLog builds the V3 Swap log a pool emits: amount0, amount1, sqrt price,
// liquidity, and tick as five words.
func swapLog(address domain.Address, logIndex uint32, sqrtPrice, liquidity, tick int64) rpc.Log {
	data := make([]byte, 0, 5*32)
	for _, value := range []int64{0, 0, sqrtPrice, liquidity, tick} {
		data = append(data, abiWord(value)...)
	}
	return rpc.Log{
		Address:  address,
		Topics:   []domain.Hash{uniswapv3.TopicSwap, {}, {}},
		Data:     data,
		LogIndex: logIndex,
	}
}

func mintLog(address domain.Address, logIndex uint32, lower, upper int64) rpc.Log {
	data := make([]byte, 0, 4*32)
	for _, value := range []int64{0, 1000, 0, 0} {
		data = append(data, abiWord(value)...)
	}
	return rpc.Log{
		Address:  address,
		Topics:   []domain.Hash{uniswapv3.TopicMint, {}, hashWord(lower), hashWord(upper)},
		Data:     data,
		LogIndex: logIndex,
	}
}

func collectLog(address domain.Address, logIndex uint32) rpc.Log {
	return rpc.Log{
		Address:  address,
		Topics:   []domain.Hash{uniswapv3.TopicCollect, {}, {}, {}},
		LogIndex: logIndex,
	}
}

func TestSelectPriorEvents(t *testing.T) {
	poolA := testPool(0x0a)
	poolB := testPool(0x0b)
	other := testAddress(0xcc)
	target := domain.Hash{0x01}

	receipts := []rpc.Receipt{
		{TransactionHash: domain.Hash{0x11}, Logs: []rpc.Log{
			swapLog(poolA.Address, 1, 100, 10, 5),
			swapLog(other, 2, 100, 10, 5),
			mintLog(poolB.Address, 3, 0, 10),
			{Address: poolA.Address, Topics: []domain.Hash{{0xde, 0xad}}, LogIndex: 4},
		}},
		{TransactionHash: domain.Hash{0x12}, Logs: []rpc.Log{
			collectLog(poolA.Address, 5),
			swapLog(poolB.Address, 6, 200, 20, 7),
		}},
		{TransactionHash: target, Logs: []rpc.Log{
			swapLog(poolA.Address, 7, 300, 30, 9),
		}},
	}
	c := caseData{Universe: []poolSpec{
		{Address: poolA.Address, Kind: poolA.Kind},
		{Address: poolB.Address, Kind: poolB.Kind},
	}}

	steps, targetIndex, err := selectPriorEvents(receipts, target, c)
	if err != nil {
		t.Fatalf("selectPriorEvents: %v", err)
	}
	if targetIndex != 2 {
		t.Fatalf("target index = %d, want 2", targetIndex)
	}

	want := []struct {
		receipt int
		log     uint32
		pool    domain.PoolKey
		kind    uniswapv3.EventKind
	}{
		{receipt: 0, log: 1, pool: poolA, kind: uniswapv3.EventSwap},
		{receipt: 0, log: 3, pool: poolB, kind: uniswapv3.EventMint},
		{receipt: 1, log: 6, pool: poolB, kind: uniswapv3.EventSwap},
	}
	if len(steps) != len(want) {
		t.Fatalf("steps = %d, want %d: %+v", len(steps), len(want), steps)
	}
	for i, check := range want {
		step := steps[i]
		if step.Receipt != check.receipt || step.Log != check.log ||
			step.Pool != check.pool || step.Event.Kind != check.kind {
			t.Errorf("step %d = receipt %d log %d pool %s kind %d, want receipt %d log %d pool %s kind %d",
				i, step.Receipt, step.Log, step.Pool.Address.Hex(), step.Event.Kind,
				check.receipt, check.log, check.pool.Address.Hex(), check.kind)
		}
	}
}

func TestSelectPriorEventsMissingTransaction(t *testing.T) {
	receipts := []rpc.Receipt{{TransactionHash: domain.Hash{0x11}}}
	_, _, err := selectPriorEvents(receipts, domain.Hash{0x22}, caseData{})
	if err == nil {
		t.Fatal("selectPriorEvents succeeded, want error")
	}
	if !strings.Contains(err.Error(), "not among") {
		t.Fatalf("error %q does not name the missing transaction", err)
	}
}

func TestSelectPriorEventsMalformedLog(t *testing.T) {
	pool := testPool(0x0a)
	target := domain.Hash{0x01}
	receipts := []rpc.Receipt{
		{TransactionHash: domain.Hash{0x11}, Logs: []rpc.Log{
			{
				Address:  pool.Address,
				Topics:   []domain.Hash{uniswapv3.TopicSwap, {}, {}},
				Data:     []byte{0x01},
				LogIndex: 9,
			},
		}},
		{TransactionHash: target},
	}

	_, _, err := selectPriorEvents(receipts, target,
		caseData{Universe: []poolSpec{{Address: pool.Address, Kind: pool.Kind}}})
	if err == nil {
		t.Fatal("selectPriorEvents succeeded, want error")
	}
	if !strings.Contains(err.Error(), "receipt 0 log 9") {
		t.Fatalf("error %q does not name the receipt and log", err)
	}
	if !strings.Contains(err.Error(), pool.Address.Hex()) {
		t.Fatalf("error %q does not name the pool", err)
	}
}

func TestUniversePools(t *testing.T) {
	v3 := poolSpec{Address: testAddress(0x01), Kind: domain.PoolKindV3}
	slipstream := poolSpec{Address: testAddress(0x02), Kind: domain.PoolKindSlipstream}

	pools, err := universePools([]poolSpec{v3, slipstream})
	if err != nil {
		t.Fatalf("universePools: %v", err)
	}
	if got := pools[v3.Address]; got.Kind != domain.PoolKindV3 {
		t.Errorf("pool %s kind = %d, want v3", v3.Address, got.Kind)
	}
	if got := pools[slipstream.Address]; got.Kind != domain.PoolKindSlipstream {
		t.Errorf("pool %s kind = %d, want slipstream", slipstream.Address, got.Kind)
	}

	unsupported := []poolSpec{{Address: testAddress(0x03), Kind: domain.PoolKindV2}}
	if _, err := universePools(unsupported); err == nil {
		t.Fatal("universePools accepted a v2 pool, want error")
	} else if !strings.Contains(err.Error(), "only v3 and slipstream") {
		t.Fatalf("error %q does not name the supported kinds", err)
	}
}

func TestSelectPriorEventsAppliesAlternativePool(t *testing.T) {
	pathPool := testPool(0x0a)
	alternative := testPool(0x0b)
	c := caseData{
		OnChain: onChainData{Path: []poolSpec{
			{Address: pathPool.Address, Kind: pathPool.Kind},
		}},
		Universe: []poolSpec{
			{Address: pathPool.Address, Kind: pathPool.Kind},
			{Address: alternative.Address, Kind: alternative.Kind},
		},
	}
	target := domain.Hash{0x01}
	receipts := []rpc.Receipt{
		{TransactionHash: domain.Hash{0x11}, Logs: []rpc.Log{
			swapLog(alternative.Address, 1, 100, 10, 5),
		}},
		{TransactionHash: target},
	}

	steps, _, err := selectPriorEvents(receipts, target, c)
	if err != nil {
		t.Fatalf("selectPriorEvents: %v", err)
	}
	if len(steps) != 1 {
		t.Fatalf("steps = %d, want 1: %+v", len(steps), steps)
	}
	if steps[0].Pool != alternative {
		t.Errorf("step pool = %s, want alternative %s",
			steps[0].Pool.Address.Hex(), alternative.Address.Hex())
	}
	if steps[0].Event.Kind != uniswapv3.EventSwap {
		t.Errorf("step event = %d, want swap", steps[0].Event.Kind)
	}
}
