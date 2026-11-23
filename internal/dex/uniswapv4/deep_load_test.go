package uniswapv4

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestLoadDeepReadsBitmapAndTicks(t *testing.T) {
	caller := newDeepCaller()
	word := new(big.Int).SetBit(new(big.Int), 5, 1) // tick -2510
	word.SetBit(word, 12, 1)                        // tick -2440
	caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(-2), "")
	caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(-1), bitmapResult(word))
	caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(0), "0x")
	caller.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(-2510), tickLiquidityResult(big.NewInt(700), big.NewInt(-700)))
	caller.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(-2440), tickLiquidityResult(big.NewInt(300), big.NewInt(300)))

	loader := &DeepLoader{Caller: caller}
	deep, err := loader.LoadDeep(context.Background(), deepTestState(), "0x10", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	if len(deep.Bitmap) != 1 {
		t.Fatalf("bitmap entries = %d, want 1", len(deep.Bitmap))
	}
	if got := deep.Bitmap[-1]; got == nil || got.Cmp(word) != 0 {
		t.Fatalf("bitmap[-1] = %v, want %s", got, word)
	}
	if len(deep.Ticks) != 2 {
		t.Fatalf("ticks = %d, want 2", len(deep.Ticks))
	}
	assertDeepTick(t, deep, -2510, 700, -700)
	assertDeepTick(t, deep, -2440, 300, 300)
	if deep.LoadedBlock.Number != 16 {
		t.Fatalf("loaded block = %d, want 16", deep.LoadedBlock.Number)
	}
	// Tick -100 at spacing 10 sits in word -1; radius 1 covers -2..0.
	if deep.WindowMin != -5120 || deep.WindowMax != 2559 {
		t.Fatalf("window = [%d, %d], want [-5120, 2559]", deep.WindowMin, deep.WindowMax)
	}

	// Sequential cost: three bitmap calls plus one call per initialized tick.
	if len(caller.calls) != 5 {
		t.Fatalf("calls = %d, want 5", len(caller.calls))
	}
	wantData := append(decodeSelector(SelectorGetTickBitmap), testPoolID[:]...)
	wantData = append(wantData, encodeSignedWord(-2)...)
	if got := hex.EncodeToString(caller.calls[0].data); got != hex.EncodeToString(wantData) {
		t.Fatalf("first bitmap call data = %s, want %s", got, hex.EncodeToString(wantData))
	}
	for i, call := range caller.calls {
		if call.to != stateViewAddress {
			t.Fatalf("call %d to %s, want StateView", i, call.to)
		}
		if call.block != "0x10" {
			t.Fatalf("call %d block = %q, want 0x10", i, call.block)
		}
	}
	if caller.batchRounds != 2 {
		t.Fatalf("batch rounds = %d, want 2", caller.batchRounds)
	}
}

func TestLoadDeepEmptyBitmap(t *testing.T) {
	caller := newDeepCaller()
	for _, position := range []int16{-2, -1, 0} {
		caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(int64(position)), "0x")
	}
	deep, err := (&DeepLoader{Caller: caller}).LoadDeep(context.Background(), deepTestState(), "latest", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	if len(deep.Bitmap) != 0 || len(deep.Ticks) != 0 {
		t.Fatalf("deep state = %d bitmap, %d ticks, want empty", len(deep.Bitmap), len(deep.Ticks))
	}
	if deep.LoadedBlock.Number != 0 {
		t.Fatalf("loaded block = %d, want 0 for a tag", deep.LoadedBlock.Number)
	}
	// The window is known even when every word is empty: the queried words
	// cover it and the empty bitmap is the value for them.
	if deep.WindowMin != -5120 || deep.WindowMax != 2559 {
		t.Fatalf("window = [%d, %d], want [-5120, 2559]", deep.WindowMin, deep.WindowMax)
	}
	if caller.batchRounds != 1 {
		t.Fatalf("batch rounds = %d, want 1 with no ticks to read", caller.batchRounds)
	}
}

func TestLoadDeepBatchAndSequentialAgree(t *testing.T) {
	word := new(big.Int).SetBit(new(big.Int), 0, 1) // tick 0 in position 0
	word.SetBit(word, 255, 1)                       // tick 2550 in position 0
	build := func() *deepFakeCaller {
		caller := newDeepCaller()
		caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(-2), "")
		caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(-1), "")
		caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(0), bitmapResult(word))
		caller.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(0), tickLiquidityResult(big.NewInt(1), big.NewInt(1)))
		caller.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(2550), tickLiquidityResult(big.NewInt(2), big.NewInt(-2)))
		return caller
	}

	batched, err := (&DeepLoader{Caller: build()}).LoadDeep(context.Background(), deepTestState(), "100", 1)
	if err != nil {
		t.Fatalf("batched LoadDeep: %v", err)
	}
	sequential, err := (&DeepLoader{Caller: sequentialCaller{fake: build()}}).
		LoadDeep(context.Background(), deepTestState(), "100", 1)
	if err != nil {
		t.Fatalf("sequential LoadDeep: %v", err)
	}
	assertDeepStateEqual(t, batched, sequential)
	if got, want := batched.LoadedBlock.Number, uint64(100); got != want {
		t.Fatalf("loaded block = %d, want %d", got, want)
	}
}

func TestLoadDeepUsesStructRadiusWhenArgumentIsZero(t *testing.T) {
	caller := newDeepCaller()
	word := new(big.Int).SetBit(new(big.Int), 0, 1) // tick -2560 in position -1
	for _, position := range []int16{-2, -1, 0} {
		out := "0x"
		if position == -1 {
			out = bitmapResult(word)
		}
		caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(int64(position)), out)
	}
	caller.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(-2560),
		tickLiquidityResult(big.NewInt(9), big.NewInt(-9)))

	loader := &DeepLoader{Caller: caller, WordRadius: 1}
	deep, err := loader.LoadDeep(context.Background(), deepTestState(), "0x1068", 0)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	assertDeepTick(t, deep, -2560, 9, -9)
	if deep.LoadedBlock.Number != 4200 {
		t.Fatalf("loaded block = %d, want 4200", deep.LoadedBlock.Number)
	}
}

func TestLoadDeepLoadedBlockStamping(t *testing.T) {
	tests := []struct {
		name  string
		block string
		want  uint64
	}{
		{"decimal", "4200", 4200},
		{"hex", "0x1068", 4200},
		{"tag", "latest", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newDeepCaller()
			for _, position := range []int16{-2, -1, 0} {
				caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(int64(position)), "0x")
			}
			deep, err := (&DeepLoader{Caller: caller}).LoadDeep(context.Background(), deepTestState(), test.block, 1)
			if err != nil {
				t.Fatalf("LoadDeep: %v", err)
			}
			if deep.LoadedBlock.Number != test.want {
				t.Fatalf("loaded block = %d, want %d", deep.LoadedBlock.Number, test.want)
			}
		})
	}
}

func TestLoadDeepMalformedResponses(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*deepFakeCaller)
	}{
		{
			name: "bitmap not hex",
			mutate: func(c *deepFakeCaller) {
				c.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(-1), "0xzz")
			},
		},
		{
			name: "bitmap short",
			mutate: func(c *deepFakeCaller) {
				c.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(-1), "0x00")
			},
		},
		{
			name: "tick liquidity short",
			mutate: func(c *deepFakeCaller) {
				c.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(0), "0x"+hex.EncodeToString(make([]byte, abiWordSize)))
			},
		},
		{
			name: "gross above uint128",
			mutate: func(c *deepFakeCaller) {
				c.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(0),
					tickLiquidityResult(pow2(128), big.NewInt(0)))
			},
		},
		{
			name: "net below int128",
			mutate: func(c *deepFakeCaller) {
				minimum := new(big.Int).Neg(pow2(127))
				minimum.Sub(minimum, big.NewInt(1))
				c.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(0),
					tickLiquidityResult(big.NewInt(1), minimum))
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newDeepCaller()
			word := new(big.Int).SetBit(new(big.Int), 0, 1) // tick 0 in position 0
			for _, position := range []int16{-2, -1, 0} {
				out := "0x"
				if position == 0 {
					out = bitmapResult(word)
				}
				caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(int64(position)), out)
			}
			caller.set(SelectorGetTickLiquidity, testPoolID, encodeSignedWord(0),
				tickLiquidityResult(big.NewInt(1), big.NewInt(1)))
			test.mutate(caller)

			_, err := (&DeepLoader{Caller: caller}).LoadDeep(context.Background(), deepTestState(), "latest", 1)
			if !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadDeepInvalidSpec(t *testing.T) {
	tests := []struct {
		name    string
		caller  func() pool.ContractCaller
		mutate  func(*pool.State)
		block   string
		radius  int16
		wantErr error
	}{
		{name: "nil caller", caller: func() pool.ContractCaller { return nil }, block: "latest", radius: 1, wantErr: ErrInvalidSpec},
		{name: "wrong kind", mutate: func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 }, block: "latest", radius: 1, wantErr: ErrUnsupportedKind},
		{name: "zero pool ID", mutate: func(s *pool.State) { s.Key.PoolID = domain.Hash{} }, block: "latest", radius: 1, wantErr: ErrInvalidSpec},
		{name: "zero tick spacing", mutate: func(s *pool.State) { s.TickSpacing = 0 }, block: "latest", radius: 1, wantErr: ErrInvalidSpec},
		{name: "negative tick spacing", mutate: func(s *pool.State) { s.TickSpacing = -10 }, block: "latest", radius: 1, wantErr: ErrInvalidSpec},
		{name: "tick above max", mutate: func(s *pool.State) { s.Tick = math.MaxTick + 1 }, block: "latest", radius: 1, wantErr: ErrInvalidSpec},
		{name: "tick below min", mutate: func(s *pool.State) { s.Tick = math.MinTick - 1 }, block: "latest", radius: 1, wantErr: ErrInvalidSpec},
		{name: "empty block", block: "", radius: 1, wantErr: ErrInvalidSpec},
		{name: "radius above maximum", block: "latest", radius: maxWordRadius + 1, wantErr: ErrInvalidSpec},
		{name: "zero radius", block: "latest", radius: 0, wantErr: ErrInvalidSpec},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := deepTestState()
			if test.mutate != nil {
				test.mutate(&state)
			}
			var caller pool.ContractCaller = newDeepCaller()
			if test.caller != nil {
				caller = test.caller()
			}
			_, err := (&DeepLoader{Caller: caller}).LoadDeep(context.Background(), state, test.block, test.radius)
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("error = %v, want %v", err, test.wantErr)
			}
		})
	}
}

func TestLoadDeepPropagatesCallerError(t *testing.T) {
	errBoom := errors.New("boom")
	caller := newDeepCaller()
	for _, position := range []int16{-2, 0} {
		caller.set(SelectorGetTickBitmap, testPoolID, encodeSignedWord(int64(position)), "")
	}
	caller.setError(SelectorGetTickBitmap, testPoolID, encodeSignedWord(-1), errBoom)
	if _, err := (&DeepLoader{Caller: caller}).LoadDeep(context.Background(), deepTestState(), "latest", 1); !errors.Is(err, errBoom) {
		t.Fatalf("error = %v, want boom", err)
	}
}

func TestLoadDeepRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&DeepLoader{Caller: newDeepCaller()}).LoadDeep(ctx, deepTestState(), "latest", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
