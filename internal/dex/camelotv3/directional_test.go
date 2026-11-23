package camelotv3

import (
	"encoding/hex"
	"math/big"
	"testing"

	poolmath "github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

// Camelot V3 uses the eight-word directional globalState ABI documented by
// Algebra commit ab8ea842204839802524f762b751ef835eb79ec3.
func directionalResult(fee0, fee1 int64) string {
	data := append(word(q96()), signedWordData(0)...)
	data = append(data, word(big.NewInt(fee0))...)
	data = append(data, word(big.NewInt(fee1))...)
	data = append(data, make([]byte, 4*abiWordSize)...)
	return "0x" + hex.EncodeToString(data)
}

func TestDirectionalGlobalStateChangesReverseQuote(t *testing.T) {
	caller := newFakeCaller()
	setHappyPath(caller, directionalResult(500, 3000), liquidityResult(big.NewInt(1_000_000_000)))
	state, err := Load(t.Context(), caller, validSpec(), "0x123")
	if err != nil {
		t.Fatal(err)
	}
	adapter, err := New(state.Fee, state.TickSpacing)
	if err != nil {
		t.Fatal(err)
	}
	deep := pool.DeepState{Bitmap: map[int16]*big.Int{}, Ticks: map[int32]pool.TickInfo{}}
	amount := big.NewInt(50_000_000)
	got, err := adapter.QuoteExactIn(t.Context(), state, deep, state.Token1, amount)
	if err != nil {
		t.Fatal(err)
	}
	want, err := poolmath.SwapV3ExactIn(poolmath.V3State{SqrtPriceX96: state.SqrtPriceX96, Tick: state.Tick, Liquidity: state.Liquidity,
		FeePips: 3000, TickSpacing: state.TickSpacing, Bitmap: deep.Bitmap, Ticks: deep.Ticks}, false, amount)
	if err != nil {
		t.Fatal(err)
	}
	if got.Cmp(want.AmountOut) != 0 {
		t.Fatalf("reverse quote used the wrong directional fee: got %s want %s", got, want.AmountOut)
	}
}

func TestDirectionalZeroFeeIsObserved(t *testing.T) {
	caller := newFakeCaller()
	setHappyPath(caller, directionalResult(0, 3000), liquidityResult(big.NewInt(1)))
	state, err := Load(t.Context(), caller, validSpec(), "0x123")
	if err != nil {
		t.Fatal(err)
	}
	if state.Fee != 0 {
		t.Fatal("observed zero fee replaced by configured fallback")
	}
}

func TestDirectionalFeeRefreshAndLayoutBounds(t *testing.T) {
	caller := newFakeCaller()
	caller.setOut(globalStateCall, directionalResult(0, 500))
	original := quoteTestState()
	reverse := uint32(9000)
	original.FeeOneForZero = &reverse
	updated, err := LoadFee(t.Context(), caller, original, "0x123")
	if err != nil {
		t.Fatal(err)
	}
	if updated.Fee != 0 || updated.FeeOneForZero == nil || *updated.FeeOneForZero != 500 || *original.FeeOneForZero != 9000 {
		t.Fatal("fee refresh lost direction, zero or isolation")
	}
	for _, out := range []string{directionalResult(500, 65536), globalStateResult(q96(), 0, big.NewInt(500), 1)} {
		caller.setOut(globalStateCall, out)
		if _, err := LoadFee(t.Context(), caller, original, "0x123"); err == nil {
			t.Fatal("invalid or ambiguous directional layout accepted")
		}
	}
}
