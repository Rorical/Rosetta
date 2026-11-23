package domain

import (
	"math/big"
	"testing"
)

func TestReceiptPriorityRequiresActualHeaderContext(t *testing.T) {
	for _, test := range []struct {
		base, price int64
		want        int64
	}{{2, 7, 5}, {7, 7, 0}, {8, 7, -1}} {
		f := &ReceiptFees{BaseFee: big.NewInt(test.base), GasPrice: big.NewInt(test.price)}
		got := f.PriorityFee()
		if test.want < 0 {
			if got != nil {
				t.Fatal(got)
			}
			continue
		}
		if got == nil || got.Int64() != test.want {
			t.Fatal(got)
		}
		if f.GasPrice.Int64() != test.price {
			t.Fatal("mutated receipt")
		}
	}
	if (&ReceiptFees{GasPrice: big.NewInt(10)}).PriorityFee() != nil {
		t.Fatal("legacy receipt invented a base fee")
	}
}
