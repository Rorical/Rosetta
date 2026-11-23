package dex

import (
	"math/big"

	"golang.org/x/crypto/sha3"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv3"
	"github.com/Rorical/Rosetta/internal/domain"
)

var swapV2Topic = swapTopic("Swap(address,uint256,uint256,uint256,uint256,address)")
var swapVelodromeTopic = swapTopic("Swap(address,address,uint256,uint256,uint256,uint256)")
var swapV4Topic = swapTopic("Swap(bytes32,address,int128,int128,uint160,uint128,int24,uint24)")

func swapTopic(signature string) (out domain.Hash) {
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(signature))
	copy(out[:], h.Sum(nil))
	return
}

// IsSwapTopic identifies the supported receipt amount formats, regardless of metadata coverage.
func IsSwapTopic(topic domain.Hash) bool {
	return topic == swapV2Topic || topic == swapVelodromeTopic || topic == swapV4Topic || topic == uniswapv3.TopicSwap || topic == uniswapv3.TopicPancakeSwap
}

// DecodeSwapAmounts returns token-flow amounts. A matched swap with nil amounts
// is ambiguous (for example, both V2 input legs); callers must not invent a route.
func DecodeSwapAmounts(kind domain.PoolKind, topics []domain.Hash, data []byte) (input, output *big.Int, zeroForOne, swap bool) {
	if len(topics) == 0 {
		return nil, nil, false, false
	}
	topic := topics[0]
	if (kind == domain.PoolKindV2 && topic == swapV2Topic || kind == domain.PoolKindVelodromeV2 && topic == swapVelodromeTopic) && len(data) == 128 {
		a, b, c, d := new(big.Int).SetBytes(data[:32]), new(big.Int).SetBytes(data[32:64]), new(big.Int).SetBytes(data[64:96]), new(big.Int).SetBytes(data[96:])
		if a.Sign() > 0 && b.Sign() == 0 && c.Sign() == 0 && d.Sign() > 0 {
			return a, d, true, true
		}
		if a.Sign() == 0 && b.Sign() > 0 && c.Sign() > 0 && d.Sign() == 0 {
			return b, c, false, true
		}
		return nil, nil, false, true
	}
	v4 := kind == domain.PoolKindV4 && topic == swapV4Topic
	v3 := (kind == domain.PoolKindV3 || kind == domain.PoolKindSlipstream || kind == domain.PoolKindCamelotV3 || kind == domain.PoolKindSolidlyV3) && topic == uniswapv3.TopicSwap
	// Pancake V3 appends two protocol fee words to the V3 amounts.
	pancake := kind == domain.PoolKindV3 && topic == uniswapv3.TopicPancakeSwap
	if pancake && len(data) == 224 {
		v3, data = true, data[:160]
	}
	if !v3 && !v4 || v3 && len(data) != 160 || v4 && len(data) != 192 {
		return nil, nil, false, false
	}
	a, b := signedSwapWord(data[:32]), signedSwapWord(data[32:64])
	if v4 {
		a.Neg(a)
		b.Neg(b)
	}
	if a.Sign() > 0 && b.Sign() < 0 {
		return a, b.Neg(b), true, true
	}
	if a.Sign() < 0 && b.Sign() > 0 {
		return b, a.Neg(a), false, true
	}
	return nil, nil, false, true
}

func signedSwapWord(word []byte) *big.Int {
	value := new(big.Int).SetBytes(word)
	if word[0]&0x80 != 0 {
		value.Sub(value, new(big.Int).Lsh(big.NewInt(1), 256))
	}
	return value
}
