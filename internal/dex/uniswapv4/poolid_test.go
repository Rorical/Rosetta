package uniswapv4

import (
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

// TestPoolIDMatchesLivePools checks the hash against two live Arbitrum One
// pools: the ETH/USDC 500/10 pool and the USDC/USDT
// 3000/60 pool behind the same PoolManager.
func TestPoolIDMatchesLivePools(t *testing.T) {
	usdc := mustParseAddress(t, "0xaf88d065e77c8cC2239327C5EDb3A432268e5831")
	tests := []struct {
		name        string
		token0      domain.Address
		token1      domain.Address
		fee         uint32
		tickSpacing int32
		want        string
	}{
		{
			name:        "native ETH / USDC 500",
			token0:      domain.Address{},
			token1:      usdc,
			fee:         500,
			tickSpacing: 10,
			want:        "0x864abca0a6202dba5b8868772308da953ff125b0f95015adbf89aaf579e903a8",
		},
		{
			name:        "native ETH / USDC 3000",
			token0:      domain.Address{},
			token1:      usdc,
			fee:         3000,
			tickSpacing: 60,
			want:        "0x03df2300e83353309c1069ae3ea89c31b361a009f9c36a1de5c8f0afcc45bde8",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := PoolID(test.token0, test.token1, test.fee, test.tickSpacing, domain.Address{})
			if err != nil {
				t.Fatalf("PoolID: %v", err)
			}
			if got.Hex() != test.want {
				t.Fatalf("PoolID = %s, want %s", got.Hex(), test.want)
			}
		})
	}
}

func TestPoolIDRejectsNegativeTickSpacing(t *testing.T) {
	if _, err := PoolID(domain.Address{}, domain.Address{0x01}, 500, -10, domain.Address{}); err == nil {
		t.Fatal("PoolID accepted a negative tick spacing")
	}
}

func mustParseAddress(t *testing.T, raw string) domain.Address {
	t.Helper()
	addr, err := domain.ParseAddress(raw)
	if err != nil {
		t.Fatalf("ParseAddress(%s): %v", raw, err)
	}
	return addr
}
