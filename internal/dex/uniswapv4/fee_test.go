package uniswapv4

import (
	"errors"
	"testing"
)

func TestSwapFeeCombinesProtocolAndLPFee(t *testing.T) {
	tests := []struct {
		name        string
		lpFee       uint32
		protocolFee uint32
		zeroForOne  bool
		want        uint32
	}{
		{
			name:        "500 lp and 125/125 zero for one",
			lpFee:       500,
			protocolFee: 0x07d07d,
			zeroForOne:  true,
			want:        625,
		},
		{
			name:        "500 lp and 125/125 one for zero",
			lpFee:       500,
			protocolFee: 0x07d07d,
			zeroForOne:  false,
			want:        625,
		},
		{
			name:        "500 lp uses the low protocol half for zero for one",
			lpFee:       500,
			protocolFee: 0x1f407d,
			zeroForOne:  true,
			want:        625,
		},
		{
			name:        "500 lp uses the high protocol half for one for zero",
			lpFee:       500,
			protocolFee: 0x1f407d,
			zeroForOne:  false,
			want:        1000,
		},
		{
			name:        "no protocol fee",
			lpFee:       3000,
			protocolFee: 0,
			zeroForOne:  true,
			want:        3000,
		},
		{
			name:        "no lp fee",
			lpFee:       0,
			protocolFee: 0x07d07d,
			zeroForOne:  true,
			want:        125,
		},
		{
			name:        "overlap is removed from the sum",
			lpFee:       100_000,
			protocolFee: 3125,
			zeroForOne:  true,
			want:        102_813,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SwapFee(test.lpFee, test.protocolFee, test.zeroForOne)
			if err != nil {
				t.Fatalf("SwapFee: %v", err)
			}
			if got != test.want {
				t.Fatalf("SwapFee = %d, want %d", got, test.want)
			}
		})
	}
}

func TestSwapFeeRejections(t *testing.T) {
	tests := []struct {
		name        string
		lpFee       uint32
		protocolFee uint32
	}{
		{name: "lp fee at denominator", lpFee: 1_000_000},
		{name: "lp fee above denominator", lpFee: 1_000_001},
		{name: "dynamic fee flag", lpFee: 0x800000},
		{name: "dynamic fee flag with a value", lpFee: 0x800000 | 500},
		{name: "protocol fee above two halves", protocolFee: 0x1000000},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := SwapFee(test.lpFee, test.protocolFee, true); !errors.Is(err, ErrInvalidFee) {
				t.Fatalf("error = %v, want ErrInvalidFee", err)
			}
		})
	}
}
