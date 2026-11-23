package camelotv3

import (
	"context"
	"encoding/hex"
	"errors"
	"math/big"
	"strings"
	"testing"

	"github.com/Rorical/Rosetta/internal/domain"
	"github.com/Rorical/Rosetta/internal/math"
	"github.com/Rorical/Rosetta/internal/pool"
)

func deepState() pool.State {
	return pool.State{
		Key:         domain.PoolKey{Address: domain.Address{0xaa}, Kind: domain.PoolKindCamelotV3},
		Token0:      domain.Address{0x01},
		Token1:      domain.Address{0x02},
		Fee:         100,
		TickSpacing: 10,
		Tick:        0,
	}
}

func tickTableResult(value *big.Int) string {
	return "0x" + hex.EncodeToString(word(value))
}

// algebraTicksResult encodes the eight-word Algebra ticks response. Only the
// first two words are read by LoadDeep.
func algebraTicksResult(gross, net int64) string {
	data := append(word(big.NewInt(gross)), signedWordData(net)...)
	data = append(data, make([]byte, abiWordSize*6)...)
	return "0x" + hex.EncodeToString(data)
}

func tickTableCallData(wordPosition int64) []byte {
	return append(append([]byte(nil), tickTableCall...), signedWordData(wordPosition)...)
}

func ticksCallData(tick int64) []byte {
	return append(append([]byte(nil), ticksCall...), signedWordData(tick)...)
}

func setEmptyTickTable(f *fakeCaller, first, last int64) {
	for word := first; word <= last; word++ {
		f.setOut(tickTableCallData(word), "0x")
	}
}

func setTickTableBits(f *fakeCaller, wordPosition int64, bits ...uint) {
	value := new(big.Int)
	for _, bit := range bits {
		value.SetBit(value, int(bit), 1)
	}
	f.setOut(tickTableCallData(wordPosition), tickTableResult(value))
}

func TestDeepSelectorsDecode(t *testing.T) {
	tests := []struct {
		name     string
		selector string
		decoded  []byte
	}{
		{"tickTable", SelectorTickTable, tickTableCall},
		{"ticks", SelectorTicks, ticksCall},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if len(test.decoded) != 4 {
				t.Fatalf("%s length = %d, want 4", test.name, len(test.decoded))
			}
			if got := "0x" + hex.EncodeToString(test.decoded); got != test.selector {
				t.Fatalf("decoded selector = %s, want %s", got, test.selector)
			}
		})
	}
}

func TestLoadDeepEmptyWords(t *testing.T) {
	fake := newFakeCaller()
	// Tick 0 at spacing 10 covers compressed words -1..1, i.e. raw words
	// -10..19.
	setEmptyTickTable(fake, -10, 19)

	deep, err := LoadDeep(context.Background(), fake, deepState(), "0x2a", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	if deep.Bitmap == nil || deep.Ticks == nil {
		t.Fatalf("deep maps = %v/%v, want non-nil empty maps", deep.Bitmap, deep.Ticks)
	}
	if len(deep.Bitmap) != 0 || len(deep.Ticks) != 0 {
		t.Fatalf("deep state = %d words/%d ticks, want empty", len(deep.Bitmap), len(deep.Ticks))
	}
	if deep.LoadedBlock.Number != 42 {
		t.Fatalf("loaded block = %d, want 42", deep.LoadedBlock.Number)
	}
	// Tick 0 at spacing 10 sits in compressed word 0; radius 1 covers -1..1.
	if deep.WindowMin != -2560 || deep.WindowMax != 5119 {
		t.Fatalf("window = [%d, %d], want [-2560, 5119]", deep.WindowMin, deep.WindowMax)
	}
	if len(fake.calls) != 30 {
		t.Fatalf("calls = %d, want 30 raw tickTable words", len(fake.calls))
	}
	for _, call := range fake.calls {
		if call.to != deepState().Key.Address || call.block != "0x2a" {
			t.Fatalf("call = (%s, %s), want (%s, 0x2a)", call.to, call.block, deepState().Key.Address)
		}
	}
}

func TestLoadDeepRepacksRawTicks(t *testing.T) {
	fake := newFakeCaller()
	setEmptyTickTable(fake, -10, -1)
	setEmptyTickTable(fake, 1, 19)
	// Raw word 0 holds raw ticks 50 and 120. At spacing 10 they compress to
	// 5 and 12, which is where internal/math expects them.
	setTickTableBits(fake, 0, 50, 120)
	fake.setOut(ticksCallData(50), algebraTicksResult(700, 700))
	fake.setOut(ticksCallData(120), algebraTicksResult(300, -300))

	deep, err := LoadDeep(context.Background(), fake, deepState(), "latest", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	table := new(big.Int).SetBit(new(big.Int), 5, 1)
	table.SetBit(table, 12, 1)
	if got := deep.Bitmap[0]; got == nil || got.Cmp(table) != 0 {
		t.Fatalf("bitmap[0] = %v, want %s", got, table)
	}
	if len(deep.Bitmap) != 1 || len(deep.Ticks) != 2 {
		t.Fatalf("deep state = %d words/%d ticks, want 1/2", len(deep.Bitmap), len(deep.Ticks))
	}
	assertTick(t, deep, 50, 700, 700)
	assertTick(t, deep, 120, 300, -300)
	if deep.LoadedBlock != (domain.BlockRef{}) {
		t.Fatalf("loaded block = %+v, want zero for a tag", deep.LoadedBlock)
	}
	if len(fake.calls) != 32 {
		t.Fatalf("calls = %d, want 32", len(fake.calls))
	}
}

func TestLoadDeepNegativeWord(t *testing.T) {
	fake := newFakeCaller()
	// Tick -100 at spacing 10 compresses to -10, so the current compressed
	// word is -1 and the raw range is -20..9.
	setEmptyTickTable(fake, -20, -2)
	setEmptyTickTable(fake, 0, 9)
	// Raw word -1 bit 246 is raw tick -10, which compresses back to -1.
	setTickTableBits(fake, -1, 246)
	fake.setOut(ticksCallData(-10), algebraTicksResult(400, -400))

	state := deepState()
	state.Tick = -100
	deep, err := LoadDeep(context.Background(), fake, state, "0x20", 1)
	if err != nil {
		t.Fatalf("LoadDeep: %v", err)
	}
	if got := deep.Bitmap[-1]; got == nil || got.Bit(255) != 1 {
		t.Fatalf("bitmap[-1] = %v, want bit 255", got)
	}
	assertTick(t, deep, -10, 400, -400)
	if deep.LoadedBlock.Number != 0x20 {
		t.Fatalf("loaded block = %d, want 32", deep.LoadedBlock.Number)
	}

	if got, want := hex.EncodeToString(callWithArgument(t, fake.calls, tickTableCall, -1).data),
		strings.TrimPrefix(SelectorTickTable, "0x")+strings.Repeat("ff", 32); got != want {
		t.Fatalf("tickTable(-1) data = %s, want %s", got, want)
	}
	if got, want := hex.EncodeToString(callWithArgument(t, fake.calls, ticksCall, -10).data),
		strings.TrimPrefix(SelectorTicks, "0x")+strings.Repeat("ff", 31)+"f6"; got != want {
		t.Fatalf("ticks(-10) data = %s, want %s", got, want)
	}
}

func TestLoadDeepMalformedResponses(t *testing.T) {
	tests := []struct {
		name  string
		table string
		ticks string
	}{
		{name: "table not hex", table: "0xzz"},
		{name: "table short word", table: "0x" + strings.Repeat("00", abiWordSize-1)},
		{name: "unaligned raw tick", table: tickTableResult(new(big.Int).SetBit(new(big.Int), 11, 1))},
		{name: "ticks empty", table: tickTableResult(new(big.Int).SetBit(new(big.Int), 10, 1)), ticks: "0x"},
		{name: "ticks one word", table: tickTableResult(new(big.Int).SetBit(new(big.Int), 10, 1)), ticks: "0x" + strings.Repeat("00", abiWordSize)},
		{name: "gross wider than uint128", table: tickTableResult(new(big.Int).SetBit(new(big.Int), 10, 1)), ticks: algebraTicksResultRaw(pow2(128), big.NewInt(0))},
		{name: "net wider than int128", table: tickTableResult(new(big.Int).SetBit(new(big.Int), 10, 1)), ticks: algebraTicksResultRaw(big.NewInt(1), pow2(128))},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeCaller()
			setEmptyTickTable(fake, -10, -1)
			setEmptyTickTable(fake, 1, 19)
			fake.setOut(tickTableCallData(0), test.table)
			fake.setOut(ticksCallData(10), test.ticks)

			if _, err := LoadDeep(context.Background(), fake, deepState(), "latest", 1); !errors.Is(err, ErrInvalidResponse) {
				t.Fatalf("error = %v, want ErrInvalidResponse", err)
			}
		})
	}
}

func TestLoadDeepCallerErrors(t *testing.T) {
	errBoom := errors.New("boom")
	tests := []struct {
		name string
		call []byte
	}{
		{name: "tickTable", call: tickTableCallData(0)},
		{name: "ticks", call: ticksCallData(10)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fake := newFakeCaller()
			setEmptyTickTable(fake, -10, -1)
			setEmptyTickTable(fake, 1, 19)
			setTickTableBits(fake, 0, 10)
			fake.setOut(ticksCallData(10), algebraTicksResult(1, 1))
			fake.setError(test.call, errBoom)

			if _, err := LoadDeep(context.Background(), fake, deepState(), "latest", 1); !errors.Is(err, errBoom) {
				t.Fatalf("error = %v, want boom", err)
			}
		})
	}
}

func TestLoadDeepInvalidState(t *testing.T) {
	tests := []struct {
		name   string
		block  string
		caller func() pool.ContractCaller
		mutate func(*pool.State)
		radius int16
	}{
		{name: "nil caller", block: "latest", caller: func() pool.ContractCaller { return nil }, radius: 1},
		{name: "zero address", block: "latest", mutate: func(s *pool.State) { s.Key.Address = domain.Address{} }, radius: 1},
		{name: "wrong kind", block: "latest", mutate: func(s *pool.State) { s.Key.Kind = domain.PoolKindV3 }, radius: 1},
		{name: "zero spacing", block: "latest", mutate: func(s *pool.State) { s.TickSpacing = 0 }, radius: 1},
		{name: "negative spacing", block: "latest", mutate: func(s *pool.State) { s.TickSpacing = -10 }, radius: 1},
		{name: "tick too high", block: "latest", mutate: func(s *pool.State) { s.Tick = 887273 }, radius: 1},
		{name: "tick too low", block: "latest", mutate: func(s *pool.State) { s.Tick = -887273 }, radius: 1},
		{name: "empty block", block: "", radius: 1},
		{name: "zero radius", block: "latest", radius: 0},
		{name: "radius too wide", block: "latest", radius: 65},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			state := deepState()
			if test.mutate != nil {
				test.mutate(&state)
			}
			var caller pool.ContractCaller = newFakeCaller()
			if test.caller != nil {
				caller = test.caller()
			}
			if _, err := LoadDeep(context.Background(), caller, state, test.block, test.radius); !errors.Is(err, ErrInvalidSpec) {
				t.Fatalf("error = %v, want ErrInvalidSpec", err)
			}
		})
	}
}

func TestLoadDeepRespectsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadDeep(ctx, newFakeCaller(), deepState(), "latest", 1); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}

func algebraTicksResultRaw(gross, net *big.Int) string {
	data := append(word(gross), word(net)...)
	data = append(data, make([]byte, abiWordSize*6)...)
	return "0x" + hex.EncodeToString(data)
}

func callWithArgument(t *testing.T, calls []recordedCall, selector []byte, argument int64) recordedCall {
	t.Helper()
	want := "0x" + hex.EncodeToString(append(append([]byte(nil), selector...), signedWordData(argument)...))
	for _, call := range calls {
		if "0x"+hex.EncodeToString(call.data) == want {
			return call
		}
	}
	t.Fatalf("no call with selector %x and argument %d", selector, argument)
	return recordedCall{}
}

func assertTick(t *testing.T, deep pool.DeepState, tick int32, gross, net int64) {
	t.Helper()
	info, ok := deep.Ticks[tick]
	if !ok {
		t.Fatalf("tick %d missing", tick)
	}
	if info.LiquidityGross.Cmp(big.NewInt(gross)) != 0 {
		t.Fatalf("tick %d gross = %s, want %d", tick, info.LiquidityGross, gross)
	}
	if info.LiquidityNet.Cmp(big.NewInt(net)) != 0 {
		t.Fatalf("tick %d net = %s, want %d", tick, info.LiquidityNet, net)
	}
}

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
