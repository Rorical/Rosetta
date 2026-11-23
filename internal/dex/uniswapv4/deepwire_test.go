package uniswapv4

import (
	"errors"
	"math/big"
	"testing"
)

func TestParseTickLiquidityBounds(t *testing.T) {
	maximumGross := new(big.Int).Sub(pow2(128), big.NewInt(1))
	minimumNet := new(big.Int).Neg(pow2(127))
	maximumNet := new(big.Int).Sub(pow2(127), big.NewInt(1))
	tests := []struct {
		name    string
		gross   *big.Int
		net     *big.Int
		wantErr bool
	}{
		{name: "maximum gross", gross: maximumGross, net: big.NewInt(0)},
		{name: "minimum net", gross: big.NewInt(0), net: minimumNet},
		{name: "maximum net", gross: big.NewInt(1), net: maximumNet},
		{name: "gross above uint128", gross: pow2(128), net: big.NewInt(0), wantErr: true},
		{name: "net below int128", gross: big.NewInt(0), net: new(big.Int).Sub(minimumNet, big.NewInt(1)), wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info, err := parseTickLiquidity(tickLiquidityResult(test.gross, test.net))
			if test.wantErr {
				if !errors.Is(err, ErrInvalidResponse) {
					t.Fatalf("error = %v, want ErrInvalidResponse", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseTickLiquidity: %v", err)
			}
			if info.LiquidityGross.Cmp(test.gross) != 0 || info.LiquidityNet.Cmp(test.net) != 0 {
				t.Fatalf("info = (%s, %s), want (%s, %s)",
					info.LiquidityGross, info.LiquidityNet, test.gross, test.net)
			}
		})
	}
}
