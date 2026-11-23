package dex

import (
	"encoding/hex"
	"testing"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
)

// Real Pancake V3 swap on Arbitrum One (WETH/USDC 0.05% pool 0xd9e2a1a6…,
// block 0x1f25171e, log index 2): 0.1077 WETH in, 229.18 USDC out.
func TestDecodeSwapAmountsPancakeV3(t *testing.T) {
	data, err := hex.DecodeString("000000000000000000000000000000000000000000000000017e504f1bb1d580fffffffffffffffffffffffffffffffffffffffffffffffffffffffff246fc2600000000000000000000000000000000000000000003082a7a17a7cb7c0fab59000000000000000000000000000000000000000000000000009f87b5230024d0fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffcf42e000000000000000000000000000000000000000000000000000010a36723b18f0000000000000000000000000000000000000000000000000000000000000000")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSwapTopic(uniswapv3.TopicPancakeSwap) {
		t.Fatal("Pancake V3 swap topic not recognized")
	}
	topics := []domain.Hash{uniswapv3.TopicPancakeSwap, {}, {}}
	input, output, zeroForOne, swap := DecodeSwapAmounts(domain.PoolKindV3, topics, data)
	if !swap || !zeroForOne || input == nil || input.Uint64() != 0x017e504f1bb1d580 || output == nil || output.Uint64() != 0x0db903da {
		t.Fatalf("input %v output %v zeroForOne %v swap %v", input, output, zeroForOne, swap)
	}
}
