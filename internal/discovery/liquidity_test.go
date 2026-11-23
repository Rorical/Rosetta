package discovery

import (
	"bytes"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
)

func TestParseFeeResult(t *testing.T) {
	tests := []struct {
		name    string
		value   string
		want    uint32
		wantErr bool
	}{
		{"typical", wordHex(big.NewInt(377)), 377, false},
		{"zero", wordHex(big.NewInt(0)), 0, false},
		{"uint24 max", wordHex(big.NewInt(0xffffff)), 0xffffff, false},
		{"uint24 overflow", wordHex(big.NewInt(0x1000000)), 0, true},
		{"short word", "0x01", 0, true},
		{"missing prefix", strings.TrimPrefix(wordHex(big.NewInt(1)), "0x"), 0, true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := parseFeeResult(test.value)
			if test.wantErr {
				if err == nil {
					t.Fatalf("parseFeeResult(%q) = %d, want error", test.value, got)
				}
				return
			}
			if err != nil || got != test.want {
				t.Fatalf("parseFeeResult(%q) = %d, %v, want %d", test.value, got, err, test.want)
			}
		})
	}
}

func TestParseLiquidityResult(t *testing.T) {
	maxUint128 := new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 128), big.NewInt(1))
	overflow := new(big.Int).Lsh(big.NewInt(1), 128)

	got, err := parseLiquidityResult(wordHex(maxUint128))
	if err != nil {
		t.Fatalf("parseLiquidityResult(max uint128): %v", err)
	}
	if got.Cmp(maxUint128) != 0 {
		t.Fatalf("liquidity = %s, want %s", got, maxUint128)
	}
	if _, err := parseLiquidityResult(wordHex(overflow)); err == nil {
		t.Fatal("parseLiquidityResult accepted a uint128 overflow")
	}
	if _, err := parseLiquidityResult("0x"); err == nil {
		t.Fatal("parseLiquidityResult accepted an empty word")
	}
}

func TestLiquidityCallData(t *testing.T) {
	v3Spec := Spec{Address: repeatedAddress(0x31), Kind: domain.PoolKindV3}
	if !bytes.Equal(liquidityCallData(v3Spec), liquidityCall) {
		t.Fatalf("v3 data = %x, want liquidity()", liquidityCallData(v3Spec))
	}
	if liquidityTarget(v3Spec) != v3Spec.Address {
		t.Fatalf("v3 target = %s, want the pool", liquidityTarget(v3Spec))
	}

	id := repeatedAddress(0x42)
	var poolID domain.Hash
	copy(poolID[:], id[:])
	v4Spec := Spec{Address: v4PoolManager, Kind: domain.PoolKindV4, PoolID: poolID}
	want := append(append([]byte(nil), getLiquidityCall...), poolID[:]...)
	if !bytes.Equal(liquidityCallData(v4Spec), want) {
		t.Fatalf("v4 data = %x, want %x", liquidityCallData(v4Spec), want)
	}
	if liquidityTarget(v4Spec) != stateView {
		t.Fatalf("v4 target = %s, want StateView %s", liquidityTarget(v4Spec), stateView)
	}
}
