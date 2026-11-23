package uniswapv3

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestLoadV3ShallowAndDeep(t *testing.T) {
	spec := loaderSpec()
	caller := newFakeCaller()
	caller.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(q96(), big.NewInt(30)))
	caller.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(big.NewInt(1_500_000)))

	bitmap := new(big.Int).SetBit(new(big.Int), 5, 1) // tick 50
	bitmap.SetBit(bitmap, 12, 1)                      // tick 120
	for _, position := range []int16{-2, -1, 0, 1, 2} {
		out := "0x"
		if position == 0 {
			out = bitmapResult(bitmap)
		}
		caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(int64(position))), out)
	}
	caller.setOut(testCallData(t, SelectorTicks, signedArgumentHex(50)), ticksResult(big.NewInt(700), big.NewInt(700)))
	caller.setOut(testCallData(t, SelectorTicks, signedArgumentHex(120)), ticksResult(big.NewInt(300), big.NewInt(-300)))

	loaded, err := Load(context.Background(), caller, spec, "0x10", 2)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if loaded.State.Key != (domain.PoolKey{Address: spec.Address, Kind: domain.PoolKindV3}) {
		t.Fatalf("key = %+v", loaded.State.Key)
	}
	if loaded.State.Token0 != spec.Token0 || loaded.State.Token1 != spec.Token1 {
		t.Fatalf("tokens = %s/%s", loaded.State.Token0, loaded.State.Token1)
	}
	if loaded.State.Fee != spec.Fee || loaded.State.TickSpacing != spec.TickSpacing {
		t.Fatalf("fee/spacing = %d/%d", loaded.State.Fee, loaded.State.TickSpacing)
	}
	if loaded.State.SqrtPriceX96.Cmp(q96()) != 0 {
		t.Fatalf("sqrt price = %s, want %s", loaded.State.SqrtPriceX96, q96())
	}
	if loaded.State.Tick != 30 {
		t.Fatalf("tick = %d, want 30", loaded.State.Tick)
	}
	assertBig(t, "liquidity", loaded.State.Liquidity, 1_500_000)

	if got := loaded.Deep.Bitmap[0]; got == nil || got.Cmp(bitmap) != 0 {
		t.Fatalf("bitmap[0] = %v, want %s", got, bitmap)
	}
	if len(loaded.Deep.Bitmap) != 1 {
		t.Fatalf("bitmap entries = %d, want 1", len(loaded.Deep.Bitmap))
	}
	assertTickInfo(t, loaded.Deep, 50, 700, 700)
	assertTickInfo(t, loaded.Deep, 120, 300, -300)
	if len(loaded.Deep.Ticks) != 2 {
		t.Fatalf("ticks entries = %d, want 2", len(loaded.Deep.Ticks))
	}

	if len(caller.calls) != 9 {
		t.Fatalf("calls = %d, want 9", len(caller.calls))
	}
	if got := hex.EncodeToString(caller.calls[0].data); got != strings.TrimPrefix(SelectorSlot0, "0x") {
		t.Fatalf("slot0 call data = %s", got)
	}
	if got := hex.EncodeToString(caller.calls[1].data); got != strings.TrimPrefix(SelectorLiquidity, "0x") {
		t.Fatalf("liquidity call data = %s", got)
	}
	assertCallArguments(t, caller.calls, SelectorTickBitmap, []int64{-2, -1, 0, 1, 2})
	assertCallArguments(t, caller.calls, SelectorTicks, []int64{50, 120})
	for i, call := range caller.calls {
		if call.to != spec.Address {
			t.Fatalf("call %d to %s, want %s", i, call.to, spec.Address)
		}
		if call.block != "0x10" {
			t.Fatalf("call %d block = %q, want 0x10", i, call.block)
		}
	}
}

func TestLoadV3NegativeWordPosition(t *testing.T) {
	spec := loaderSpec()
	caller := newFakeCaller()
	caller.setOut(testCallData(t, SelectorSlot0, ""), slot0Result(q96(), big.NewInt(-100)))
	caller.setOut(testCallData(t, SelectorLiquidity, ""), liquidityResult(big.NewInt(500_000)))

	bitmap := new(big.Int).Lsh(big.NewInt(1), 255) // word -1, bit 255 -> tick -10
	setEmptyBitmap(t, caller, -2, 0)
	caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(-1)), bitmapResult(bitmap))
	caller.setOut(testCallData(t, SelectorTicks, signedArgumentHex(-10)), ticksResult(big.NewInt(400), big.NewInt(-400)))

	loaded, err := Load(context.Background(), caller, spec, "0x20", 1)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.State.Tick != -100 {
		t.Fatalf("tick = %d, want -100", loaded.State.Tick)
	}
	if got := loaded.Deep.Bitmap[-1]; got == nil || got.Bit(255) != 1 {
		t.Fatalf("bitmap[-1] = %v, want bit 255", got)
	}
	assertTickInfo(t, loaded.Deep, -10, 400, -400)
	if len(loaded.Deep.Ticks) != 1 {
		t.Fatalf("ticks entries = %d, want 1", len(loaded.Deep.Ticks))
	}

	if len(caller.calls) != 6 {
		t.Fatalf("calls = %d, want 6", len(caller.calls))
	}
	assertCallArguments(t, caller.calls, SelectorTickBitmap, []int64{-2, -1, 0})
	assertCallArguments(t, caller.calls, SelectorTicks, []int64{-10})
	if got, want := hex.EncodeToString(callWithArgument(t, caller.calls, SelectorTickBitmap, -1).data),
		strings.TrimPrefix(SelectorTickBitmap, "0x")+strings.Repeat("ff", 32); got != want {
		t.Fatalf("tickBitmap(-1) call data = %s, want %s", got, want)
	}
	if got, want := hex.EncodeToString(callWithArgument(t, caller.calls, SelectorTicks, -10).data),
		strings.TrimPrefix(SelectorTicks, "0x")+strings.Repeat("ff", 31)+"f6"; got != want {
		t.Fatalf("ticks(-10) call data = %s, want %s", got, want)
	}
}

func TestLoadV3EmptyBitmapWord(t *testing.T) {
	caller := validLoaderCaller(t)
	loaded, err := Load(context.Background(), caller, loaderSpec(), "0x30", 1)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Deep.Bitmap) != 0 || len(loaded.Deep.Ticks) != 0 {
		t.Fatalf("deep state = %d bitmap, %d ticks, want empty", len(loaded.Deep.Bitmap), len(loaded.Deep.Ticks))
	}
	if len(caller.calls) != 5 {
		t.Fatalf("calls = %d, want 5", len(caller.calls))
	}
}

func TestLoadV3TickCallFailure(t *testing.T) {
	errTicks := errors.New("ticks unavailable")
	caller := validLoaderCaller(t)
	caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), bitmapResult(big.NewInt(2)))
	caller.setError(testCallData(t, SelectorTicks, signedArgumentHex(10)), errTicks)

	_, err := Load(context.Background(), caller, loaderSpec(), "0x50", 1)
	if !errors.Is(err, errTicks) {
		t.Fatalf("error = %v, want %v", err, errTicks)
	}
}

func TestEncodeSignedWord(t *testing.T) {
	tests := []struct {
		name  string
		value int64
		want  string
	}{
		{"zero", 0, strings.Repeat("00", 32)},
		{"one", 1, strings.Repeat("00", 31) + "01"},
		{"minus one", -1, strings.Repeat("ff", 32)},
		{"minus ten", -10, strings.Repeat("ff", 31) + "f6"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := hex.EncodeToString(encodeSignedWord(test.value)); got != test.want {
				t.Fatalf("encoding = %s, want %s", got, test.want)
			}
		})
	}
}

func assertTickInfo(t *testing.T, deep pool.DeepState, tick int32, gross, net int64) {
	t.Helper()
	info, ok := deep.Ticks[tick]
	if !ok {
		t.Fatalf("tick %d missing", tick)
	}
	assertBig(t, fmt.Sprintf("tick %d gross", tick), info.LiquidityGross, gross)
	assertBig(t, fmt.Sprintf("tick %d net", tick), info.LiquidityNet, net)
}
