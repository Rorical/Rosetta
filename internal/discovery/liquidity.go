package discovery

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"

	"github.com/Rorical/Rosetta/internal/dex/uniswapv2"
	"github.com/Rorical/Rosetta/internal/domain"
)

// Read-only selectors used to size a candidate pool. Each is a four-byte ABI
// selector, verified against the deployed contracts.
var (
	feeCall          = []byte{0xdd, 0xca, 0x3f, 0x43} // fee()
	liquidityCall    = []byte{0x1a, 0x68, 0x65, 0x02} // liquidity()
	getLiquidityCall = []byte{0xfa, 0x67, 0x93, 0xd5} // StateView.getLiquidity(bytes32)
)

// liquidityTarget is the contract that reports active liquidity. V4 pools
// share the PoolManager, which does not expose the lens, so the read goes
// through StateView.
func liquidityTarget(spec Spec, deployments ...*domain.Deployments) domain.Address {
	if spec.Kind == domain.PoolKindV4 {
		if len(deployments) > 0 && deployments[0] != nil {
			return deployments[0].V4StateView
		}
		return stateView
	}
	return spec.Address
}

// liquidityCallData is liquidity() for the V3-shaped pools and
// getLiquidity(poolId) for V4.
func liquidityCallData(spec Spec) []byte {
	if spec.Kind == domain.PoolKindV2 || spec.Kind == domain.PoolKindVelodromeV2 {
		return []byte{0x09, 0x02, 0xf1, 0xac}
	}
	if spec.Kind != domain.PoolKindV4 {
		return liquidityCall
	}
	data := make([]byte, len(getLiquidityCall)+len(spec.PoolID))
	copy(data, getLiquidityCall)
	copy(data[len(getLiquidityCall):], spec.PoolID[:])
	return data
}

func parsePoolLiquidity(spec Spec, output string) (*big.Int, error) {
	if spec.Kind == domain.PoolKindVelodromeV2 {
		raw, err := hex.DecodeString(strings.TrimPrefix(output, "0x"))
		if err != nil || len(raw) != 96 {
			return nil, fmt.Errorf("invalid Velodrome reserves")
		}
		return new(big.Int).Sqrt(new(big.Int).Mul(new(big.Int).SetBytes(raw[:32]), new(big.Int).SetBytes(raw[32:64]))), nil
	}
	if spec.Kind != domain.PoolKindV2 {
		return parseLiquidityResult(output)
	}
	reserve0, reserve1, err := uniswapv2.ParseReserves(output)
	if err != nil {
		return nil, err
	}
	// sqrt(x*y) is the constant-product liquidity proxy in raw token units.
	// It is not a token-normalized USD valuation.
	return new(big.Int).Sqrt(new(big.Int).Mul(reserve0, reserve1)), nil
}

// resultWord decodes one 0x-prefixed ABI word returned by eth_call.
func resultWord(field, value string) ([]byte, error) {
	raw, ok := strings.CutPrefix(value, "0x")
	if !ok {
		return nil, fmt.Errorf("parse %s %q: missing 0x prefix", field, value)
	}
	word, err := hex.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s %q: %w", field, value, err)
	}
	if len(word) != abiWordSize {
		return nil, fmt.Errorf("parse %s %q: got %d bytes, want %d", field, value, len(word), abiWordSize)
	}
	return word, nil
}

// parseFeeResult decodes fee() as one uint24 word.
func parseFeeResult(value string) (uint32, error) {
	word, err := resultWord("fee", value)
	if err != nil {
		return 0, err
	}
	for _, b := range word[:abiWordSize-3] {
		if b != 0 {
			return 0, fmt.Errorf("parse fee %q: value exceeds uint24", value)
		}
	}
	return uint32(word[29])<<16 | uint32(word[30])<<8 | uint32(word[31]), nil
}

// parseLiquidityResult decodes liquidity() or getLiquidity() as one uint128
// word.
func parseLiquidityResult(value string) (*big.Int, error) {
	word, err := resultWord("liquidity", value)
	if err != nil {
		return nil, err
	}
	for _, b := range word[:abiWordSize-16] {
		if b != 0 {
			return nil, fmt.Errorf("parse liquidity %q: value exceeds uint128", value)
		}
	}
	return new(big.Int).SetBytes(word[abiWordSize-16:]), nil
}
