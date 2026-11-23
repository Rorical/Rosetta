package uniswapv3

import (
	"context"
	"errors"
	"math/big"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func TestTickWindow(t *testing.T) {
	tests := []struct {
		name    string
		first   int64
		last    int64
		spacing int32
		wantMin int32
		wantMax int32
	}{
		{"single word", 0, 0, 10, 0, 2559},
		{"radius one", -1, 1, 10, -2560, 5119},
		{"wide spacing", 0, 0, 60, 0, 15359},
		{"upper clamp", 3464, 3466, 1, 886784, math.MaxTick},
		{"lower clamp", -3467, -3465, 1, math.MinTick, -886785},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotMin, gotMax := tickWindow(test.first, test.last, test.spacing)
			if gotMin != test.wantMin || gotMax != test.wantMax {
				t.Fatalf("tickWindow(%d, %d, %d) = [%d, %d], want [%d, %d]",
					test.first, test.last, test.spacing, gotMin, gotMax, test.wantMin, test.wantMax)
			}
		})
	}
}

func deepLoaderState(kind domain.PoolKind) pool.State {
	spec := loaderSpec()
	return pool.State{
		Key:          domain.PoolKey{Address: spec.Address, Kind: kind},
		Token0:       spec.Token0,
		Token1:       spec.Token1,
		Fee:          spec.Fee,
		TickSpacing:  spec.TickSpacing,
		SqrtPriceX96: q96(),
		Tick:         30,
		Liquidity:    big.NewInt(1_000_000),
	}
}

// setTwoTickBitmap serves empty words at the given positions and one word at
// position 0 holding ticks 50 and 120 for tick spacing 10.
func setTwoTickBitmap(t *testing.T, setter responseSetter, empty ...int16) *big.Int {
	t.Helper()
	bitmap := new(big.Int).SetBit(new(big.Int), 5, 1)
	bitmap.SetBit(bitmap, 12, 1)
	for _, position := range empty {
		setter.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(int64(position))), "0x")
	}
	setter.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), bitmapResult(bitmap))
	setter.setOut(testCallData(t, SelectorTicks, signedArgumentHex(50)), ticksResult(big.NewInt(700), big.NewInt(700)))
	setter.setOut(testCallData(t, SelectorTicks, signedArgumentHex(120)), ticksResult(big.NewInt(300), big.NewInt(-300)))
	return bitmap
}

func TestDeepLoaderOneWordTwoTicks(t *testing.T) {
	caller := newFakeBatchCaller()
	bitmap := setTwoTickBitmap(t, caller, -1, 1)

	deep, err := (&DeepLoader{Caller: caller}).LoadDeep(
		context.Background(), deepLoaderState(domain.PoolKindV3), "0x10", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}

	if got := deep.Bitmap[0]; got == nil || got.Cmp(bitmap) != 0 {
		t.Fatalf("bitmap[0] = %v, want %s", got, bitmap)
	}
	if len(deep.Bitmap) != 1 {
		t.Fatalf("bitmap entries = %d, want 1", len(deep.Bitmap))
	}
	if len(deep.Ticks) != 2 {
		t.Fatalf("tick entries = %d, want 2", len(deep.Ticks))
	}
	assertTickInfo(t, deep, 50, 700, 700)
	assertTickInfo(t, deep, 120, 300, -300)
	if deep.LoadedBlock.Number != 16 {
		t.Fatalf("loaded block = %d, want 16", deep.LoadedBlock.Number)
	}
	if deep.LoadedBlock.Hash != (domain.Hash{}) {
		t.Fatalf("loaded block hash = %s, want zero", deep.LoadedBlock.Hash)
	}
	// Tick 30 at spacing 10 sits in word 0, and radius 1 covers words -1..1.
	if deep.WindowMin != -2560 || deep.WindowMax != 5119 {
		t.Fatalf("window = [%d, %d], want [-2560, 5119]", deep.WindowMin, deep.WindowMax)
	}

	// No slot0 or liquidity read: the caller already has the shallow state.
	if len(caller.batches) != 2 {
		t.Fatalf("batches = %d, want 2", len(caller.batches))
	}
	if got := len(caller.batches[0]); got != 3 {
		t.Fatalf("bitmap batch = %d calls, want 3", got)
	}
	if got := len(caller.batches[1]); got != 2 {
		t.Fatalf("ticks batch = %d calls, want 2", got)
	}
	for _, batch := range caller.batches {
		for _, call := range batch {
			if call.To != loaderSpec().Address {
				t.Fatalf("call to %s, want %s", call.To, loaderSpec().Address)
			}
			if call.Block != "0x10" {
				t.Fatalf("call block = %q, want 0x10", call.Block)
			}
		}
	}
}

func TestDeepLoaderEmptyWord(t *testing.T) {
	caller := newFakeCaller()
	setEmptyBitmap(t, caller, -1, 0, 1)

	deep, err := (&DeepLoader{Caller: caller}).LoadDeep(
		context.Background(), deepLoaderState(domain.PoolKindV3), "0x20", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	if deep.Bitmap == nil || deep.Ticks == nil {
		t.Fatal("empty deep state has nil maps")
	}
	if len(deep.Bitmap) != 0 || len(deep.Ticks) != 0 {
		t.Fatalf("deep state = %d bitmap, %d ticks, want empty", len(deep.Bitmap), len(deep.Ticks))
	}
	if len(caller.calls) != 3 {
		t.Fatalf("calls = %d, want 3 bitmap reads", len(caller.calls))
	}
	if deep.LoadedBlock.Number != 32 {
		t.Fatalf("loaded block = %d, want 32", deep.LoadedBlock.Number)
	}
}

func TestDeepLoaderCallErrors(t *testing.T) {
	bitmapErr := errors.New("bitmap unavailable")
	ticksErr := errors.New("ticks unavailable")
	tests := []struct {
		name    string
		prepare func(t *testing.T, caller *fakeCaller)
		want    error
	}{
		{
			name: "bitmap call fails",
			prepare: func(t *testing.T, caller *fakeCaller) {
				t.Helper()
				caller.setError(testCallData(t, SelectorTickBitmap, signedArgumentHex(-1)), bitmapErr)
			},
			want: bitmapErr,
		},
		{
			name: "ticks call fails",
			prepare: func(t *testing.T, caller *fakeCaller) {
				t.Helper()
				setEmptyBitmap(t, caller, -1, 1)
				caller.setOut(testCallData(t, SelectorTickBitmap, signedArgumentHex(0)), bitmapResult(big.NewInt(2)))
				caller.setError(testCallData(t, SelectorTicks, signedArgumentHex(10)), ticksErr)
			},
			want: ticksErr,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeCaller()
			test.prepare(t, caller)

			_, err := (&DeepLoader{Caller: caller}).LoadDeep(
				context.Background(), deepLoaderState(domain.PoolKindV3), "0x30", 1)
			if !errors.Is(err, test.want) {
				t.Fatalf("error = %v, want %v", err, test.want)
			}
		})
	}
}

func TestDeepLoaderStampsLoadedBlock(t *testing.T) {
	tests := []struct {
		name  string
		block string
		want  uint64
	}{
		{"hex", "0x10", 16},
		{"upper hex prefix", "0X20", 32},
		{"decimal", "1000", 1000},
		{"zero", "0x0", 0},
		{"tag latest", "latest", 0},
		{"tag finalized", "finalized", 0},
		{"bare hex prefix", "0x", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeCaller()
			setEmptyBitmap(t, caller, -1, 0, 1)

			deep, err := (&DeepLoader{Caller: caller}).LoadDeep(
				context.Background(), deepLoaderState(domain.PoolKindV3), test.block, 1)
			if err != nil {
				t.Fatalf("LoadDeep: %v", err)
			}
			if deep.LoadedBlock.Number != test.want {
				t.Fatalf("loaded block = %d, want %d", deep.LoadedBlock.Number, test.want)
			}
			for _, call := range caller.calls {
				if call.block != test.block {
					t.Fatalf("call block = %q, want %q", call.block, test.block)
				}
			}
		})
	}
}

func TestDeepLoaderSequentialFallback(t *testing.T) {
	caller := newFakeCaller()
	setTwoTickBitmap(t, caller, -1, 1)

	deep, err := (&DeepLoader{Caller: caller}).LoadDeep(
		context.Background(), deepLoaderState(domain.PoolKindV3), "0x40", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	assertTickInfo(t, deep, 50, 700, 700)
	assertTickInfo(t, deep, 120, 300, -300)

	selectors := make([]string, len(caller.calls))
	for i, call := range caller.calls {
		selectors[i] = selectorOf(call.data)
	}
	// Sequential loading reads each word and its ticks before moving to the next
	// word, so the ticks of word 0 fall between the neighboring bitmap reads.
	want := []string{SelectorTickBitmap, SelectorTickBitmap, SelectorTicks, SelectorTicks, SelectorTickBitmap}
	if len(selectors) != len(want) {
		t.Fatalf("calls = %v, want %v", selectors, want)
	}
	for i := range want {
		if selectors[i] != want[i] {
			t.Fatalf("call %d = %s, want %s", i, selectors[i], want[i])
		}
	}
	assertCallArguments(t, caller.calls, SelectorTickBitmap, []int64{-1, 0, 1})
	assertCallArguments(t, caller.calls, SelectorTicks, []int64{50, 120})
}

func TestDeepLoaderWordRadius(t *testing.T) {
	tests := []struct {
		name     string
		loader   int16
		argument int16
		words    int16
	}{
		{"argument wins", 1, 2, 5},
		{"loader default", 2, 0, 5},
		{"single word", 1, 1, 3},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeCaller()
			setEmptyBitmap(t, caller, -2, -1, 0, 1, 2)

			_, err := (&DeepLoader{Caller: caller, WordRadius: test.loader}).LoadDeep(
				context.Background(), deepLoaderState(domain.PoolKindV3), "0x50", test.argument)
			if err != nil {
				t.Fatalf("LoadDeep: %v", err)
			}
			if got := int16(len(caller.calls)); got != test.words {
				t.Fatalf("bitmap reads = %d, want %d", got, test.words)
			}
		})
	}
}

func TestDeepLoaderRejectsUnsupportedKind(t *testing.T) {
	tests := []struct {
		name string
		kind domain.PoolKind
	}{
		{"v2", domain.PoolKindV2},
		{"camelot v3", domain.PoolKindCamelotV3},
		{"v4", domain.PoolKindV4},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeCaller()

			_, err := (&DeepLoader{Caller: caller}).LoadDeep(
				context.Background(), deepLoaderState(test.kind), "0x60", 1)
			if !errors.Is(err, ErrUnsupportedKind) {
				t.Fatalf("error = %v, want ErrUnsupportedKind", err)
			}
			if len(caller.calls) != 0 {
				t.Fatalf("calls = %d, want none", len(caller.calls))
			}
		})
	}
}

func TestDeepLoaderRejectsInvalidState(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*pool.State)
		block  string
		radius int16
	}{
		{"zero pool address", func(s *pool.State) { s.Key.Address = domain.Address{} }, "0x1", 1},
		{"zero token0", func(s *pool.State) { s.Token0 = domain.Address{} }, "0x1", 1},
		{"zero token1", func(s *pool.State) { s.Token1 = domain.Address{} }, "0x1", 1},
		{"equal tokens", func(s *pool.State) { s.Token1 = s.Token0 }, "0x1", 1},
		{"fee at denominator", func(s *pool.State) { s.Fee = maxFeePips }, "0x1", 1},
		{"zero tick spacing", func(s *pool.State) { s.TickSpacing = 0 }, "0x1", 1},
		{"negative tick spacing", func(s *pool.State) { s.TickSpacing = -10 }, "0x1", 1},
		{"empty block", func(*pool.State) {}, "", 1},
		{"zero radius", func(*pool.State) {}, "0x1", 0},
		{"radius too large", func(*pool.State) {}, "0x1", 65},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			caller := newFakeCaller()
			state := deepLoaderState(domain.PoolKindV3)
			test.mutate(&state)

			_, err := (&DeepLoader{Caller: caller}).LoadDeep(context.Background(), state, test.block, test.radius)
			if !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want ErrInvalidSpec", err)
			}
			if len(caller.calls) != 0 {
				t.Fatalf("calls = %d, want none", len(caller.calls))
			}
		})
	}
}

func TestDeepLoaderNilCaller(t *testing.T) {
	_, err := (&DeepLoader{}).LoadDeep(context.Background(), deepLoaderState(domain.PoolKindV3), "0x1", 1)
	if !errors.Is(err, ErrInvalidSpec) {
		t.Fatalf("error = %v, want ErrInvalidSpec", err)
	}
}

func TestDeepLoaderCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	caller := newFakeCaller()

	_, err := (&DeepLoader{Caller: caller}).LoadDeep(ctx, deepLoaderState(domain.PoolKindV3), "0x1", 1)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
	if len(caller.calls) != 0 {
		t.Fatalf("calls = %d, want none", len(caller.calls))
	}
}

func TestDeepLoaderSlipstreamKind(t *testing.T) {
	caller := newFakeCaller()
	setEmptyBitmap(t, caller, -1, 0, 1)

	if _, err := (&DeepLoader{Caller: caller}).LoadDeep(
		context.Background(), deepLoaderState(domain.PoolKindSlipstream), "0x70", 1); err != nil {
		t.Fatalf("LoadDeep slipstream: %v", err)
	}
}
